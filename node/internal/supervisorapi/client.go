package supervisorapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

type Client struct {
	StateDir     string
	Endpoint     string
	Token        string
	HTTPClient   *http.Client
	PollInterval time.Duration
}

func Discover(stateDir string) (*Client, error) {
	paths, err := controlPaths(stateDir)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{paths.endpoint, paths.token} {
			info, err := os.Lstat(path)
			if err != nil {
				return nil, err
			}
			if info.Mode().Perm()&0077 != 0 {
				return nil, fmt.Errorf("Supervisor API discovery file has unsafe permissions: %s", path)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("Supervisor API discovery file cannot be a symlink: %s", path)
			}
		}
	}
	endpointBytes, err := os.ReadFile(paths.endpoint)
	if err != nil {
		return nil, err
	}
	tokenBytes, err := os.ReadFile(paths.token)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimSpace(string(endpointBytes))
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return nil, fmt.Errorf("invalid Supervisor API endpoint")
	}
	host := net.ParseIP(parsed.Hostname())
	if host == nil || !host.IsLoopback() || parsed.Port() == "" {
		return nil, fmt.Errorf("Supervisor API endpoint is not loopback-only")
	}
	token := strings.TrimSpace(string(tokenBytes))
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("invalid Supervisor API token")
	}
	return &Client{
		StateDir:     paths.stateDir,
		Endpoint:     endpoint,
		Token:        token,
		HTTPClient:   &http.Client{Timeout: 15 * time.Second},
		PollInterval: 100 * time.Millisecond,
	}, nil
}

func (client *Client) RequestUpdate(ctx context.Context, version string) (OperationStatus, error) {
	body, err := json.Marshal(map[string]string{"version": version})
	if err != nil {
		return OperationStatus{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.Endpoint+"/v1/update", bytes.NewReader(body))
	if err != nil {
		return OperationStatus{}, err
	}
	request.Header.Set("Authorization", "Bearer "+client.Token)
	request.Header.Set("Content-Type", "application/json")
	return client.do(request, http.StatusAccepted)
}

func (client *Client) Status(ctx context.Context) (OperationStatus, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.Endpoint+"/v1/update/status", nil)
	if err != nil {
		return OperationStatus{}, err
	}
	request.Header.Set("Authorization", "Bearer "+client.Token)
	return client.do(request, http.StatusOK)
}

func (client *Client) do(request *http.Request, wantedStatus int) (OperationStatus, error) {
	httpClient := client.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return OperationStatus{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return OperationStatus{}, ErrNoOperation
	}
	if response.StatusCode != wantedStatus {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		if response.StatusCode == http.StatusConflict {
			return OperationStatus{}, ErrUpdateInProgress
		}
		return OperationStatus{}, fmt.Errorf("Supervisor API returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	var status OperationStatus
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&status); err != nil {
		return OperationStatus{}, err
	}
	return status, nil
}

func (client *Client) Wait(ctx context.Context, operationID string) (OperationStatus, error) {
	if operationID == "" {
		return OperationStatus{}, fmt.Errorf("operation ID is required")
	}
	delay := client.PollInterval
	if delay <= 0 {
		delay = 100 * time.Millisecond
	}
	for {
		status, err := client.Status(ctx)
		if err == nil {
			if status.OperationID != operationID {
				return OperationStatus{}, ErrOperationChanged
			}
			if status.Phase.terminal() {
				return status, nil
			}
		} else if !errors.Is(err, ErrNoOperation) {
			// A successful Supervisor handoff changes the endpoint and token. The
			// atomic status file remains the local source of continuity while the
			// successor starts listening.
			if client.StateDir != "" {
				if paths, pathErr := controlPaths(client.StateDir); pathErr == nil {
					if persisted, statusErr := readStatus(paths.status); statusErr == nil {
						if persisted.OperationID != operationID {
							return OperationStatus{}, ErrOperationChanged
						}
						if persisted.Phase.terminal() {
							return persisted, nil
						}
					}
				}
				if refreshed, discoverErr := Discover(client.StateDir); discoverErr == nil {
					client.Endpoint, client.Token, client.HTTPClient = refreshed.Endpoint, refreshed.Token, refreshed.HTTPClient
				}
			} else {
				return OperationStatus{}, err
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return OperationStatus{}, ctx.Err()
		case <-timer.C:
		}
	}
}
