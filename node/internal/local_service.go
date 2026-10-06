package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ssine/mira/node/internal/installation"
	"github.com/ssine/mira/node/internal/supervisorapi"
)

// Lifecycle requests use the protected local control endpoint, never a PID file.
func serviceRequest(ctx context.Context, dir, method, route string) (map[string]any, error) {
	client, err := supervisorapi.Discover(dir)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, client.Endpoint+route, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+client.Token)
	response, err := client.HTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 && response.StatusCode != 202 {
		return nil, fmt.Errorf("local Supervisor returned HTTP %d", response.StatusCode)
	}
	var result map[string]any
	if response.StatusCode == 200 {
		err = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&result)
	}
	return result, err
}

func runLocalService(ctx context.Context, action string, args []string) (any, error) {
	dir, err := defaultSupervisorStateDir()
	if err != nil {
		return nil, err
	}
	flags := flagSet(action)
	stateDir := flags.String("state-dir", dir, "Mira installation state directory")
	if err = flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("unexpected service arguments")
	}
	state, err := installation.LoadState(nil, *stateDir)
	if err != nil {
		return nil, err
	}
	if state.ServiceManager != installation.ServiceManagerBuiltin {
		return nil, fmt.Errorf("installation uses %s; use its service manager", state.ServiceManager)
	}
	if action == "status" {
		result, statusErr := serviceRequest(ctx, *stateDir, "GET", "/v1/service")
		if statusErr != nil {
			if !serviceAbsent(statusErr) {
				return nil, statusErr
			}
			return map[string]any{"status": "stopped", "serviceManager": "builtin", "stateDir": *stateDir}, nil
		}
		return result, nil
	}
	unlock, err := lockBuiltinStart(ctx, *stateDir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	running, statusErr := serviceRequest(ctx, *stateDir, "GET", "/v1/service")
	if statusErr != nil && !serviceAbsent(statusErr) {
		return nil, statusErr
	}
	if action == "stop" || action == "restart" {
		if statusErr == nil {
			if _, err = serviceRequest(ctx, *stateDir, "POST", "/v1/service/stop"); err != nil {
				return nil, err
			}
			until := time.NewTimer(45 * time.Second)
			defer until.Stop()
			for {
				if _, e := serviceRequest(ctx, *stateDir, "GET", "/v1/service"); e != nil {
					break
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-until.C:
					return nil, fmt.Errorf("Supervisor did not stop")
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
		if action == "stop" {
			return map[string]any{"status": "stopped", "stateDir": *stateDir}, nil
		}
	} else if statusErr == nil {
		return running, nil
	}
	executable := filepath.Join(*stateDir, "current", "mira")
	supervisorArgs := []string{"supervisor", "--state-dir", *stateDir, "--service-owner", "mira"}
	if state.Role == installation.RoleServer {
		supervisorArgs = append(supervisorArgs, "--server")
	}
	if err = ValidateSupervisorCandidate(supervisorArgs[1:]); err != nil {
		return nil, err
	}
	if err = detachSupervisor(executable, supervisorArgs); err != nil {
		return nil, err
	}
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		if result, e := serviceRequest(ctx, *stateDir, "GET", "/v1/service"); e == nil {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("Supervisor failed to start; inspect %s", filepath.Join(*stateDir, "supervisor.log"))
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func supervisorRuntimeView(dir string, manager installation.ServiceManager) any {
	return map[string]any{"status": "running", "serviceManager": manager, "pid": os.Getpid(), "version": Version, "stateDir": dir}
}

func serviceAbsent(err error) bool {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var network *net.OpError
	return errors.As(err, &network) && errors.Is(network.Err, syscall.ECONNREFUSED)
}
