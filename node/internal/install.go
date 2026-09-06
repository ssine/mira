package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ssine/mira/node/internal/installation"
	"github.com/ssine/mira/node/internal/supervisor"
	"github.com/ssine/mira/node/internal/supervisorapi"
)

var releaseVersionPattern = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`)

// Compare stable SemVer without integer overflow, including multi-digit components.
func compareReleaseVersions(left, right string) int {
	l, r := strings.Split(left, "."), strings.Split(right, ".")
	for i := 0; i < 3; i++ {
		if len(l[i]) > len(r[i]) {
			return 1
		}
		if len(l[i]) < len(r[i]) {
			return -1
		}
		if order := strings.Compare(l[i], r[i]); order != 0 {
			return order
		}
	}
	return 0
}

// effectiveUpdateTarget preserves the no-implicit-downgrade behavior of
// `mira update` while still letting the Supervisor finish an interrupted
// handoff. In that recovery window current already selects the new image, but
// install-state (and on Windows the service BinaryPath) can still name the old
// one, so a same-version request must not be treated as a no-op.
func effectiveUpdateTarget(requested, target, current, recorded string) (string, bool) {
	if requested == "latest" && releaseVersionPattern.MatchString(current) && compareReleaseVersions(target, current) < 0 {
		target = current
	}
	return target, target == current && recorded == current
}

func defaultConfigFile() (string, error) {
	identity, err := DefaultIdentityFile()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(identity), "node.json"), nil
}

func runSetup(args []string) (any, error) {
	defaultPath, err := defaultConfigFile()
	if err != nil {
		return nil, err
	}
	set := flagSet("setup")
	server := set.String("server", "", "Mira Server URL")
	configPath := set.String("config", defaultPath, "Node configuration file")
	defaultIdentity, err := DefaultIdentityFile()
	if err != nil {
		return nil, err
	}
	identityFlag := set.String("identity", defaultIdentity, "Node identity file")
	if err := set.Parse(args); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(strings.TrimRight(*server, "/"))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("setup requires --server with an absolute HTTP(S) Mira URL")
	}
	serverURL := strings.TrimRight(parsed.String(), "/")
	identityPath := *identityFlag
	if !filepath.IsAbs(identityPath) {
		return nil, fmt.Errorf("identity path must be absolute")
	}
	if !filepath.IsAbs(*configPath) {
		return nil, fmt.Errorf("configuration path must be absolute")
	}
	if identity, err := loadIdentity(identityPath); err == nil && identity.ServerURL != serverURL {
		return nil, fmt.Errorf("this Node identity belongs to %s; refusing to rebind it to another Server", identity.ServerURL)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if content, err := os.ReadFile(*configPath); err == nil {
		var existing fileConfig
		if err := json.Unmarshal(content, &existing); err != nil {
			return nil, fmt.Errorf("read existing configuration: %w", err)
		}
		if strings.TrimRight(existing.ServerURL, "/") != serverURL {
			return nil, fmt.Errorf("existing configuration uses %s; not overwriting it", existing.ServerURL)
		}
		return map[string]any{"status": "configured", "serverUrl": serverURL, "configFile": *configPath, "preserved": true}, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(*configPath), 0700); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(filepath.Dir(*configPath), ".node-config-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := protectIdentityFile(file); err != nil {
		return nil, err
	}
	configuration := map[string]any{"serverUrl": serverURL, "identityFile": identityPath, "appServerAutoStart": false}
	if err := json.NewEncoder(file).Encode(configuration); err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(file.Name(), *configPath); err != nil {
		return nil, err
	}
	return map[string]any{"status": "configured", "serverUrl": serverURL, "configFile": *configPath, "preserved": false}, nil
}

func localStatus(ctx context.Context, options cliOptions) (any, error) {
	identity, err := loadIdentity(options.Identity)
	if os.IsNotExist(err) {
		configuration, _ := defaultConfigFile()
		return map[string]any{"status": "not_started", "configFile": configuration, "hint": "Start mira node-worker to submit an enrollment request"}, nil
	}
	if err != nil {
		return nil, err
	}
	view := identityView(identity, options.Identity)
	view["build"] = CurrentBuild()
	if identity.Enrollment.Status != "approved" {
		view["verificationCode"] = identity.Enrollment.VerificationCode
		view["hint"] = "Open the Server website and approve this Node after checking its verification code"
		return view, nil
	}
	client, err := newCLIClient(options)
	if err != nil {
		return nil, err
	}
	var node map[string]any
	if err := client.request(ctx, http.MethodGet, "/v1/nodes/"+identity.NodeID, nil, &node); err != nil {
		view["serverReachable"], view["connectionError"] = false, err.Error()
	} else {
		view["serverReachable"], view["connectionStatus"] = true, node["status"]
		view["nodeVersion"], view["lastSeenAt"] = node["nodeVersion"], node["lastSeenAt"]
	}
	return view, nil
}

func latestRelease(ctx context.Context) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/ssine/mira/releases/latest", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "mira/"+Version)
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub release lookup returned HTTP %d", response.StatusCode)
	}
	var release struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&release); err != nil {
		return "", err
	}
	version := strings.TrimPrefix(release.Tag, "v")
	if !releaseVersionPattern.MatchString(version) {
		return "", fmt.Errorf("GitHub returned an invalid release version")
	}
	return version, nil
}

func runUpdate(ctx context.Context, options cliOptions, args []string, stdin io.Reader, stdout, stderr io.Writer) (any, error) {
	_ = stdin
	_ = stderr
	defaultState, err := defaultSupervisorStateDir()
	if err != nil {
		return nil, err
	}
	set := flagSet("update")
	check := set.Bool("check", false, "only check for a release")
	noWait := set.Bool("no-wait", false, "return after Supervisor accepts the update")
	stateDir := set.String("state-dir", defaultState, "Mira state directory")
	requested := set.String("version", "latest", "target release version")
	if err := set.Parse(args); err != nil {
		return nil, err
	}
	target := strings.TrimPrefix(*requested, "v")
	if target == "latest" {
		target, err = latestRelease(ctx)
	}
	if err != nil {
		return nil, err
	}
	if !releaseVersionPattern.MatchString(target) {
		return nil, fmt.Errorf("--version must be a semantic version or latest")
	}
	state, err := installation.LoadState(nil, *stateDir)
	if err != nil {
		return nil, fmt.Errorf("read Mira installation: %w", err)
	}
	layout, err := supervisor.NewLayout(*stateDir)
	if err != nil {
		return nil, err
	}
	supervisorState, err := layout.ReadState()
	if err != nil {
		return nil, fmt.Errorf("read Supervisor state: %w", err)
	}
	if supervisorState.ServiceOwner != state.ServiceOwner {
		return nil, fmt.Errorf("%w: install state says %s but Supervisor says %s", installation.ErrOwnershipConflict, state.ServiceOwner, supervisorState.ServiceOwner)
	}
	current := supervisorState.Current
	if *check {
		available := current != target
		if releaseVersionPattern.MatchString(current) {
			available = compareReleaseVersions(target, current) > 0
		}
		return map[string]any{"currentVersion": current, "targetVersion": target, "updateAvailable": available, "releaseUrl": "https://github.com/ssine/mira/releases/tag/v" + target, "serviceOwner": state.ServiceOwner}, nil
	}
	target, settled := effectiveUpdateTarget(*requested, target, current, state.Version)
	if settled {
		return map[string]any{"status": "up_to_date", "version": current, "serviceOwner": state.ServiceOwner}, nil
	}
	client, err := supervisorapi.Discover(*stateDir)
	if err != nil {
		return nil, fmt.Errorf("connect to local Mira Supervisor: %w", err)
	}
	operation, err := client.RequestUpdate(ctx, target)
	if err != nil {
		return nil, err
	}
	if !options.JSON {
		fmt.Fprintf(stdout, "Mira Supervisor accepted update %s -> %s (%s).\n", current, target, operation.OperationID)
	}
	if *noWait {
		return map[string]any{"status": "accepted", "operation": operation, "serviceOwner": state.ServiceOwner}, nil
	}
	status, err := client.Wait(ctx, operation.OperationID)
	if err != nil {
		return nil, fmt.Errorf("wait for Supervisor update: %w; the update continues independently", err)
	}
	result := map[string]any{"status": status.Phase, "operation": status, "serviceOwner": state.ServiceOwner}
	if status.Phase == supervisorapi.PhaseSucceeded {
		return result, nil
	}
	if status.Error == "" {
		status.Error = "Mira update did not succeed"
	}
	return result, fmt.Errorf("%s: %s", status.Phase, status.Error)
}
