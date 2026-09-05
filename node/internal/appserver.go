package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type codexInstallation struct {
	Path                       string `json:"path"`
	Version                    string `json:"version,omitempty"`
	SHA256                     string `json:"sha256,omitempty"`
	AppServerSupported         bool   `json:"appServerSupported"`
	RemoteThreadStoreSupported bool   `json:"remoteThreadStoreSupported"`
	ValidationError            string `json:"validationError,omitempty"`
	ValidatedAt                string `json:"validatedAt"`
}

type desiredAppServer struct {
	Running         bool     `json:"running"`
	ListenURL       string   `json:"listenUrl"`
	CodexPath       string   `json:"codexPath"`
	CodexHome       string   `json:"codexHome"`
	ConfigOverrides []string `json:"configOverrides"`
	Revision        int64    `json:"revision"`
}

type appServerInstance struct {
	command            *exec.Cmd
	codex              codexInstallation
	requestedListenURL string
	listenURL          string
	codexHome          string
	configOverrides    []string
	startedAt          time.Time
	ready              bool
	done               chan struct{}
	output             outputBuffer
}

type appServerManager struct {
	configuration    config
	mu               sync.Mutex
	nodeToken        string
	discovered       bool
	installations    []codexInstallation
	instance         *appServerInstance
	lastError        string
	runtimePreparing bool
	runtimeCancel    context.CancelFunc
	runtimeRetryAt   time.Time
	runtimeInstall   func(context.Context) (string, error) // injectable preparation for tests
}

func (manager *appServerManager) setNodeCredential(token string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.nodeToken = token
}

func newAppServerManager(configuration config) *appServerManager {
	return &appServerManager{configuration: configuration}
}

func supportsAppServer() bool {
	return runtime.GOOS != "android"
}

func (manager *appServerManager) discover(ctx context.Context) error {
	manager.mu.Lock()
	if manager.discovered {
		manager.mu.Unlock()
		return nil
	}
	manager.discovered = true
	manager.mu.Unlock()
	if !supportsAppServer() {
		return nil
	}
	paths := managedCodexCandidates(manager.configuration.CodexBinary, manager.configuration.IdentityFile)
	seen := map[string]bool{}
	installations := []codexInstallation{}
	for _, candidate := range paths {
		absolute, err := filepath.Abs(candidate)
		if err != nil || seen[absolute] {
			continue
		}
		seen[absolute] = true
		installation := inspectCodex(ctx, absolute)
		installations = append(installations, installation)
		Log("inspected Codex installation", map[string]any{
			"path": absolute, "version": installation.Version,
			"appServerSupported":         installation.AppServerSupported,
			"remoteThreadStoreSupported": installation.RemoteThreadStoreSupported,
		})
	}
	manager.mu.Lock()
	manager.installations = installations
	manager.mu.Unlock()
	return nil
}

func inspectCodex(parent context.Context, path string) codexInstallation {
	installation := codexInstallation{Path: path, ValidatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	versionOutput, versionErr := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	helpOutput, helpErr := exec.CommandContext(ctx, path, "app-server", "--help").CombinedOutput()
	if versionErr != nil || helpErr != nil {
		installation.ValidationError = strings.TrimSpace(fmt.Sprintf("version: %v; app-server help: %v", versionErr, helpErr))
		return installation
	}
	installation.Version = strings.TrimSpace(string(versionOutput))
	help := string(helpOutput)
	installation.AppServerSupported = strings.Contains(help, "--listen") && strings.Contains(help, "generate-json-schema")
	installation.RemoteThreadStoreSupported = supportsRemoteThreadStore(ctx, path)
	file, err := os.Open(path)
	if err == nil {
		defer file.Close()
		hash := sha256.New()
		if _, err := io.Copy(hash, file); err == nil {
			installation.SHA256 = hex.EncodeToString(hash.Sum(nil))
		}
	}
	return installation
}

func supportsRemoteThreadStore(ctx context.Context, path string) bool {
	probe := exec.CommandContext(ctx, path,
		"-c", `experimental_thread_store.type="remote_http"`,
		"-c", `experimental_thread_store.endpoint="http://127.0.0.1:9"`,
		"-c", `experimental_thread_store.store_id="mira-probe"`,
		"features", "list",
	)
	return probe.Run() == nil
}

func (manager *appServerManager) installationsView() []codexInstallation {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return append([]codexInstallation(nil), manager.installations...)
}

func (manager *appServerManager) selectCodex(ctx context.Context, desired desiredAppServer) *codexInstallation {
	requiresRemoteStore := false
	for _, override := range desired.ConfigOverrides {
		if strings.HasPrefix(override, `experimental_thread_store.type="remote_http"`) {
			requiresRemoteStore = true
			break
		}
	}
	eligible := func(installation *codexInstallation) bool {
		return installation.AppServerSupported && (!requiresRemoteStore || installation.RemoteThreadStoreSupported)
	}
	if desired.CodexPath != "" {
		absolute, err := filepath.Abs(desired.CodexPath)
		if err == nil {
			for index := range manager.installations {
				if manager.installations[index].Path == absolute {
					if eligible(&manager.installations[index]) {
						return &manager.installations[index]
					}
					return nil
				}
			}
			installation := inspectCodex(ctx, absolute)
			manager.installations = append(manager.installations, installation)
			for index := range manager.installations {
				if manager.installations[index].Path == absolute {
					if eligible(&manager.installations[index]) {
						return &manager.installations[index]
					}
					return nil
				}
			}
		}
		return nil
	}
	for index := range manager.installations {
		if eligible(&manager.installations[index]) {
			return &manager.installations[index]
		}
	}
	return nil
}

func (manager *appServerManager) effectiveDesired(desired desiredAppServer) desiredAppServer {
	if desired.ListenURL == "" {
		desired.ListenURL = manager.configuration.AppServerListenURL
	}
	if desired.CodexHome == "" {
		desired.CodexHome = manager.configuration.AppServerCodexHome
	}
	// Server-provided overrides are deliberately non-secret. Local overrides are
	// appended last so credentials stay on the Node and cannot be replaced by
	// central desired state.
	desired.ConfigOverrides = append(
		append([]string(nil), desired.ConfigOverrides...),
		manager.configuration.ConfigOverrides...,
	)
	return desired
}

func (manager *appServerManager) reconcile(ctx context.Context, desired desiredAppServer) error {
	if !supportsAppServer() {
		return nil
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	desired = manager.effectiveDesired(desired)
	if !desired.Running {
		if manager.runtimeCancel != nil {
			manager.runtimeCancel()
		}
		return manager.stopLocked()
	}
	if desired.CodexPath == "" && manager.configuration.CodexBinary == "" {
		// Unconfigured managed execution always uses the pinned Mira runtime, not
		// whichever unrelated Codex happens to sort first on PATH.
		store, err := newCodexRuntimeStore(manager.configuration.IdentityFile)
		if err != nil {
			manager.lastError = err.Error()
			return err
		}
		binary, err := store.cached()
		if err != nil {
			manager.prepareRuntimeLocked(ctx, store)
			return nil
		}
		desired.CodexPath = binary
	}
	selected := manager.selectCodex(ctx, desired)
	if selected == nil || !selected.AppServerSupported {
		manager.lastError = "no validated Codex App Server installation found"
		return fmt.Errorf("%s", manager.lastError)
	}
	if manager.instance != nil && !channelClosed(manager.instance.done) {
		current := manager.instance
		if current.requestedListenURL == desired.ListenURL && current.codex.Path == selected.Path && current.codexHome == desired.CodexHome && stringSlicesEqual(current.configOverrides, desired.ConfigOverrides) {
			return nil
		}
		if err := manager.stopLocked(); err != nil {
			return err
		}
	}
	return manager.startLocked(ctx, desired, *selected)
}

// Do not hold the manager lock or delay the reverse-channel heartbeat while a
// large optional runtime downloads. A later reconciliation starts the process.
func (manager *appServerManager) prepareRuntimeLocked(parent context.Context, store *codexRuntimeStore) {
	if manager.runtimePreparing || time.Now().Before(manager.runtimeRetryAt) {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	manager.runtimePreparing, manager.runtimeCancel = true, cancel
	manager.lastError = "preparing Codex runtime " + store.identity.Version
	install := manager.runtimeInstall
	if install == nil {
		install = func(ctx context.Context) (string, error) { return store.ensure(ctx, io.Discard) }
	}
	go func() {
		defer cancel()
		binary, err := install(ctx)
		var installation codexInstallation
		if err == nil {
			installation = inspectCodex(ctx, binary)
			if !installation.AppServerSupported || !installation.RemoteThreadStoreSupported {
				err = fmt.Errorf("downloaded Codex failed App Server/remote ThreadStore validation: %s", installation.ValidationError)
			}
		}
		manager.mu.Lock()
		defer manager.mu.Unlock()
		manager.runtimePreparing, manager.runtimeCancel = false, nil
		if err != nil {
			manager.lastError = "Codex runtime: " + err.Error()
			if ctx.Err() == nil {
				manager.runtimeRetryAt = time.Now().Add(time.Minute)
			}
			return
		}
		manager.runtimeRetryAt, manager.lastError = time.Time{}, ""
		manager.installations = append([]codexInstallation{installation}, manager.installations...)
	}()
}

func channelClosed(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validateAppServerListenURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "ws" || parsed.Port() == "" {
		return fmt.Errorf("App Server listen URL must be ws://loopback:PORT")
	}
	hostname := parsed.Hostname()
	if hostname != "localhost" {
		ip := net.ParseIP(hostname)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("App Server must listen on loopback")
		}
	}
	return nil
}

func availableAppServerListenURL(value string) (string, error) {
	if err := validateAppServerListenURL(value); err != nil {
		return "", err
	}
	parsed, _ := url.Parse(value)
	listener, err := net.Listen("tcp", parsed.Host)
	if err == nil {
		defer listener.Close()
		if parsed.Port() != "0" {
			return value, nil
		}
	} else if !appServerAddressInUse(err) {
		return "", fmt.Errorf("probe App Server listen URL: %w", err)
	}
	if listener == nil {
		listener, err = net.Listen("tcp", net.JoinHostPort(parsed.Hostname(), "0"))
		if err != nil {
			return "", fmt.Errorf("allocate App Server loopback port: %w", err)
		}
		defer listener.Close()
	}
	port := listener.Addr().(*net.TCPAddr).Port
	parsed.Host = net.JoinHostPort(parsed.Hostname(), strconv.Itoa(port))
	return parsed.String(), nil
}

func appServerAddressInUse(err error) bool {
	// Winsock reports WSAEADDRINUSE (10048), not the POSIX EADDRINUSE
	// value exported by syscall on Windows.
	return errors.Is(err, syscall.EADDRINUSE) ||
		(runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(10048)))
}

func appServerFailureMessage(instance *appServerInstance, fallback string) string {
	view := instance.output.read(0)
	chunks, _ := view["chunks"].([]outputChunk)
	var output strings.Builder
	for _, chunk := range chunks {
		output.WriteString(chunk.Text)
	}
	message := strings.TrimSpace(output.String())
	if len(message) > 4096 {
		message = strings.ToValidUTF8(message[len(message)-4096:], "�")
	}
	if message == "" {
		return fallback
	}
	return fallback + ": " + message
}

func (manager *appServerManager) startLocked(ctx context.Context, desired desiredAppServer, codex codexInstallation) error {
	listenURL, err := availableAppServerListenURL(desired.ListenURL)
	if err != nil {
		manager.lastError = err.Error()
		return err
	}
	arguments := []string{"app-server", "--listen", listenURL}
	for _, override := range desired.ConfigOverrides {
		arguments = append(arguments, "-c", override)
	}
	command := exec.Command(codex.Path, arguments...)
	command.Env = os.Environ()
	if manager.nodeToken != "" {
		// The patched ThreadStore reads this environment variable when
		// bearer_token is omitted. It intentionally never appears in argv.
		command.Env = append(command.Env, "MIRA_NODE_TOKEN="+manager.nodeToken)
	}
	if desired.CodexHome != "" {
		if err := os.MkdirAll(desired.CodexHome, 0700); err != nil {
			manager.lastError = fmt.Sprintf("create Codex home: %v", err)
			return fmt.Errorf("%s", manager.lastError)
		}
		command.Env = append(command.Env, "CODEX_HOME="+desired.CodexHome)
	}
	instance := &appServerInstance{
		command: command, codex: codex, requestedListenURL: desired.ListenURL, listenURL: listenURL,
		codexHome: desired.CodexHome, configOverrides: append([]string(nil), desired.ConfigOverrides...),
		startedAt: time.Now().UTC(), done: make(chan struct{}),
	}
	command.Stdout = streamWriter{buffer: &instance.output, stream: "stdout"}
	command.Stderr = streamWriter{buffer: &instance.output, stream: "stderr"}
	if err := command.Start(); err != nil {
		manager.lastError = err.Error()
		return err
	}
	manager.instance = instance
	go func() {
		_ = command.Wait()
		instance.output.flush()
		close(instance.done)
	}()
	manager.mu.Unlock()
	err = waitForAppServer(ctx, instance)
	manager.mu.Lock()
	if err != nil {
		manager.lastError = appServerFailureMessage(instance, err.Error())
		_ = terminateProcess(command.Process, "SIGTERM")
		return fmt.Errorf("%s", manager.lastError)
	}
	instance.ready = true
	manager.lastError = ""
	Log("started Codex App Server", map[string]any{"listenUrl": listenURL, "requestedListenUrl": desired.ListenURL, "codexPath": codex.Path, "pid": command.Process.Pid})
	return nil
}

func appServerHealthURL(listenURL string) string {
	parsed, _ := url.Parse(listenURL)
	parsed.Scheme = "http"
	parsed.Path = "/healthz"
	parsed.RawQuery = ""
	return parsed.String()
}

func waitForAppServer(ctx context.Context, instance *appServerInstance) error {
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	healthURL := appServerHealthURL(instance.listenURL)
	client := &http.Client{Timeout: time.Second}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-instance.done:
			return fmt.Errorf("App Server exited before becoming ready")
		case <-deadline.C:
			return fmt.Errorf("App Server did not become ready at %s", healthURL)
		case <-ticker.C:
			response, err := client.Get(healthURL)
			if err == nil {
				response.Body.Close()
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					return nil
				}
			}
		}
	}
}

func (manager *appServerManager) stopLocked() error {
	instance := manager.instance
	if instance == nil {
		return nil
	}
	manager.instance = nil
	if channelClosed(instance.done) {
		return nil
	}
	if err := terminateProcess(instance.command.Process, "SIGTERM"); err != nil {
		if !channelClosed(instance.done) {
			return err
		}
	}
	manager.mu.Unlock()
	select {
	case <-instance.done:
	case <-time.After(5 * time.Second):
		_ = terminateProcess(instance.command.Process, "SIGKILL")
		<-instance.done
	}
	manager.mu.Lock()
	Log("stopped Codex App Server", nil)
	return nil
}

func (manager *appServerManager) report() map[string]any {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if !supportsAppServer() {
		return map[string]any{"status": "unsupported"}
	}
	miraCLIPath := localMiraCLIPath()
	instance := manager.instance
	if instance == nil || channelClosed(instance.done) {
		status := "stopped"
		if manager.runtimePreparing {
			status = "starting"
		}
		return map[string]any{
			"status": status, "lastError": manager.lastError,
			"runtimePreparing": manager.runtimePreparing,
			"miraCliPath":      miraCLIPath,
		}
	}
	status := "starting"
	if instance.ready {
		status = "running"
	}
	return map[string]any{
		"status": status, "pid": instance.command.Process.Pid, "listenUrl": instance.listenURL,
		"codexPath": instance.codex.Path, "codexVersion": instance.codex.Version,
		"miraCliPath": miraCLIPath,
		"codexHome":   instance.codexHome, "configOverrideCount": len(instance.configOverrides),
		"startedAt": instance.startedAt.Format(time.RFC3339Nano), "lastError": manager.lastError,
	}
}

func (manager *appServerManager) readyListenURL() (string, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.instance == nil || !manager.instance.ready || channelClosed(manager.instance.done) {
		return "", false
	}
	return manager.instance.listenURL, true
}

func (manager *appServerManager) close() {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.runtimeCancel != nil {
		manager.runtimeCancel()
	}
	_ = manager.stopLocked()
}
