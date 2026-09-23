//go:build android

package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type androidBridge struct {
	url    string
	token  string
	client *http.Client
}

func newAndroidBridge(url string, token string) *androidBridge {
	return &androidBridge{
		url: url, token: token,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (bridge *androidBridge) screen(ctx context.Context, params screenParams) (any, error) {
	return bridge.call(ctx, "/v1/screen", params)
}

func (bridge *androidBridge) deliverCompletion(ctx context.Context, raw json.RawMessage, serverURL string) (any, error) {
	if bridge == nil {
		return nil, fmt.Errorf("Android app bridge unavailable")
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("invalid completion")
	}
	value["serverUrl"] = serverURL
	return bridge.call(ctx, "/v1/notification", value)
}

func (bridge *androidBridge) call(ctx context.Context, path string, params any) (any, error) {
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, bridge.url+path, bytes.NewReader(encoded),
	)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+bridge.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := bridge.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Android app bridge: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 16*1024*1024))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(payload, &body)
		if body.Error == "" {
			body.Error = string(payload)
		}
		return nil, fmt.Errorf("Android app bridge returned %d: %s", response.StatusCode, body.Error)
	}
	var result any
	if err := json.Unmarshal(payload, &result); err != nil {
		return nil, fmt.Errorf("decode Android app bridge response: %w", err)
	}
	return result, nil
}
