package node

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ssine/mira/node/internal/installation"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/supervisor"
	"github.com/ssine/mira/node/internal/supervisorapi"
)

func defaultSupervisorStateDir() (string, error) {
	if value := os.Getenv("MIRA_STATE_DIR"); value != "" {
		if !filepath.IsAbs(value) {
			return "", fmt.Errorf("MIRA_STATE_DIR must be absolute")
		}
		return filepath.Clean(value), nil
	}
	if executable, err := os.Executable(); err == nil {
		if stateDir, found := installedStateDir(executable); found {
			return stateDir, nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(home, ".mira"), nil
	}
	return filepath.Join(home, ".local", "share", "mira"), nil
}

func installedStateDir(executable string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", false
	}
	versionDir := filepath.Dir(resolved)
	versionsDir := filepath.Dir(versionDir)
	if filepath.Base(versionsDir) != "versions" || filepath.Base(versionDir) == "" {
		return "", false
	}
	stateDir := filepath.Dir(versionsDir)
	info, err := os.Stat(filepath.Join(stateDir, "install-state.json"))
	if err != nil || info.IsDir() {
		return "", false
	}
	return filepath.Clean(stateDir), true
}

type supervisorRuntimeOptions struct {
	stateDir           string
	owner              supervisor.ServiceOwner
	serverRole         bool
	windowsServiceName string
}

func parseSupervisorRuntimeOptions(args []string, command string) (supervisorRuntimeOptions, error) {
	defaultState, err := defaultSupervisorStateDir()
	if err != nil {
		return supervisorRuntimeOptions{}, err
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	stateDir := flags.String("state-dir", defaultState, "Mira installation state directory")
	ownerValue := flags.String("service-owner", os.Getenv("MIRA_SERVICE_OWNER"), "system service owner: nix or mira")
	serverRole := flags.Bool("server", strings.EqualFold(os.Getenv("MIRA_SERVER_ROLE"), "true"), "also run Mira Server")
	windowsServiceName := flags.String("windows-service-name", "Mira", "Windows Service name")
	if err := flags.Parse(args); err != nil {
		return supervisorRuntimeOptions{}, err
	}
	if flags.NArg() != 0 {
		return supervisorRuntimeOptions{}, fmt.Errorf("unexpected %s arguments: %s", command, strings.Join(flags.Args(), " "))
	}
	layout, err := supervisor.NewLayout(*stateDir)
	if err != nil {
		return supervisorRuntimeOptions{}, err
	}
	owner := supervisor.ServiceOwner(*ownerValue)
	if owner == "" {
		state, stateErr := layout.ReadState()
		if stateErr != nil {
			return supervisorRuntimeOptions{}, fmt.Errorf("read installation ownership (run mira install first or pass --service-owner): %w", stateErr)
		}
		owner = state.ServiceOwner
	}
	return supervisorRuntimeOptions{stateDir: layout.StateDir, owner: owner, serverRole: *serverRole, windowsServiceName: *windowsServiceName}, nil
}

// ValidateSupervisorCandidate is run from the staged image while the old
// Supervisor still owns the update. It exercises the successor's flag parser,
// ownership state and worker configuration without acquiring the live lock or
// starting a second Server.
func ValidateSupervisorCandidate(args []string) error {
	options, err := parseSupervisorRuntimeOptions(args, "supervisor-check")
	if err != nil {
		return err
	}
	layout, err := supervisor.NewLayout(options.stateDir)
	if err != nil {
		return err
	}
	state, err := layout.ReadState()
	if err != nil {
		return fmt.Errorf("read Supervisor state: %w", err)
	}
	if state.ServiceOwner != options.owner {
		return fmt.Errorf("Supervisor service owner is %s, not %s", state.ServiceOwner, options.owner)
	}
	if _, err := loadConfigArgs([]string{"--config", filepath.Join(options.stateDir, "node.json")}); err != nil {
		return fmt.Errorf("validate Node worker configuration: %w", err)
	}
	healthChecker := supervisor.HTTPHealthChecker{}
	if options.serverRole {
		serverConfig, err := foundation.LoadConfig()
		if err != nil {
			return err
		}
		healthChecker.ServerURL = "http://" + net.JoinHostPort(serverConfig.ListenHost, strconv.Itoa(serverConfig.ListenPort))
	}
	_, err = supervisor.New(supervisor.Config{
		StateDir: options.stateDir, ServiceOwner: options.owner,
		Node: supervisor.WorkerConfig{Args: []string{"node-worker", "--config", filepath.Join(options.stateDir, "node.json")}},
		Server: func() *supervisor.WorkerConfig {
			if options.serverRole {
				return &supervisor.WorkerConfig{Args: []string{"server-worker"}}
			}
			return nil
		}(),
		Launcher: supervisor.ExecLauncher{}, Stager: supervisor.GitHubStager{}, Validator: supervisor.CandidateValidator{}, HealthChecker: healthChecker,
	})
	return err
}

// RunSupervisor is the long-lived outer process. The Supervisor never depends
// on a live Server, Node channel, or initiating SSH connection to finish or
// roll back an update.
func RunSupervisor(ctx context.Context, args []string) error {
	options, err := parseSupervisorRuntimeOptions(args, "supervisor")
	if err != nil {
		return err
	}
	healthChecker := supervisor.HTTPHealthChecker{}
	if options.serverRole {
		serverConfig, configErr := foundation.LoadConfig()
		if configErr != nil {
			return configErr
		}
		healthHost := serverConfig.ListenHost
		if healthHost == "" || healthHost == "0.0.0.0" || healthHost == "::" {
			healthHost = "127.0.0.1"
		}
		healthChecker.ServerURL = "http://" + net.JoinHostPort(healthHost, strconv.Itoa(serverConfig.ListenPort))
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	configuration := supervisor.Config{
		StateDir: options.stateDir, ServiceOwner: options.owner,
		Node:     supervisor.WorkerConfig{Args: []string{"node-worker", "--config", filepath.Join(options.stateDir, "node.json")}},
		Launcher: supervisor.ExecLauncher{}, Stager: supervisor.GitHubStager{},
		Validator: supervisor.CandidateValidator{SelfCheckArgs: append([]string{
			"supervisor-check", "--state-dir", options.stateDir, "--service-owner", string(options.owner),
		}, func() []string {
			if options.serverRole {
				return []string{"--server"}
			}
			return nil
		}()...)},
		HealthChecker: healthChecker,
		GracePeriod:   15 * time.Second, RestartDelay: time.Second, UpdateTimeout: 2 * time.Minute,
	}
	configuration.Handoff = func(handoffContext context.Context, candidate supervisor.Candidate) error {
		if err := installation.RecordCurrentVersion(handoffContext, options.stateDir, options.owner, candidate.Version); err != nil {
			return fmt.Errorf("record activated Mira version: %w", err)
		}
		// current already points at the validated release. Exiting now lets the
		// owning service manager restart the Supervisor from that stable path.
		cancel()
		return nil
	}
	if options.serverRole {
		configuration.Server = &supervisor.WorkerConfig{Args: []string{"server-worker"}}
	}
	manager, err := supervisor.New(configuration)
	if err != nil {
		return err
	}
	if err := manager.Start(runContext); err != nil {
		return err
	}
	control, err := supervisorapi.Start(supervisorapi.Config{StateDir: options.stateDir, Manager: manager})
	if err != nil {
		stopContext, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		stopErr := manager.Stop(stopContext)
		stopCancel()
		if stopErr != nil {
			return errors.Join(err, stopErr)
		}
		return err
	}
	defer func() {
		closeContext, closeCancel := context.WithTimeout(context.Background(), 35*time.Second)
		if closeErr := control.Close(closeContext); closeErr != nil {
			Log("close Supervisor control API", map[string]any{"error": closeErr.Error()})
		}
		closeCancel()
	}()
	err = manager.Run(runContext)
	if err == context.Canceled && ctx.Err() == nil {
		// Exit unsuccessfully so on-failure service managers (notably Windows
		// Service recovery) start the successor selected by current.
		return ErrSupervisorHandoff
	}
	return err
}
