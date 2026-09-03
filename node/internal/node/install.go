package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
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
	if err := set.Parse(args); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(strings.TrimRight(*server, "/"))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("setup requires --server with an absolute HTTP(S) Mira URL")
	}
	serverURL := strings.TrimRight(parsed.String(), "/")
	identityPath, err := DefaultIdentityFile()
	if err != nil {
		return nil, err
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
		return map[string]any{"status": "not_started", "configFile": configuration, "hint": "Start mira-node to submit an enrollment request"}, nil
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

func updatePreflight(ctx context.Context, options cliOptions) error {
	client, err := newCLIClient(options)
	if err != nil {
		if os.IsNotExist(err) || cliExitCode(err) == 2 {
			return nil
		}
		return err
	}
	var node map[string]any
	if err := client.request(ctx, http.MethodGet, "/v1/nodes/"+client.identity.NodeID, nil, &node); err != nil {
		return err
	}
	if reported, ok := node["reportedAppServer"].(map[string]any); ok && (reported["status"] == "running" || reported["status"] == "starting") {
		return fmt.Errorf("Codex App Server is active; stop it first or explicitly use --force")
	}
	if node["status"] != "online" {
		return nil
	}
	for _, capability := range []string{"process", "pty"} {
		result, _, err := client.invoke(ctx, client.identity.NodeID, capability, map[string]any{"action": "list"})
		if err != nil {
			return err
		}
		view, _ := result.(map[string]any)
		key := "processes"
		if capability == "pty" {
			key = "sessions"
		}
		items, _ := view[key].([]any)
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			if item["running"] == true {
				return fmt.Errorf("active %s session exists; close it first or explicitly use --force", capability)
			}
		}
	}
	return nil
}

func runUpdate(ctx context.Context, options cliOptions, args []string, stdin io.Reader, stdout, stderr io.Writer) (any, error) {
	set := flagSet("update")
	check := set.Bool("check", false, "only check for a release")
	force := set.Bool("force", false, "allow active sessions to be interrupted")
	requested := set.String("version", "latest", "target release version")
	if err := set.Parse(args); err != nil {
		return nil, err
	}
	target := strings.TrimPrefix(*requested, "v")
	var err error
	if target == "latest" {
		target, err = latestRelease(ctx)
	}
	if err != nil {
		return nil, err
	}
	if !releaseVersionPattern.MatchString(target) {
		return nil, fmt.Errorf("--version must be a semantic version or latest")
	}
	if *check {
		return map[string]any{"currentVersion": Version, "targetVersion": target, "updateAvailable": compareReleaseVersions(target, Version) > 0, "releaseUrl": "https://github.com/ssine/mira/releases/tag/v" + target}, nil
	}
	if options.JSON {
		return nil, fmt.Errorf("--json is supported for update --check, not an interactive update")
	}
	if (target == Version && !*force) || (*requested == "latest" && compareReleaseVersions(target, Version) < 0) {
		return map[string]any{"status": "up_to_date", "version": Version}, nil
	}
	if !*force {
		if err := updatePreflight(ctx, options); err != nil {
			return nil, fmt.Errorf("update preflight: %w", err)
		}
	}
	var command *exec.Cmd
	executable, _ := os.Executable()
	installRoot := filepath.Dir(filepath.Dir(filepath.Dir(executable)))
	if filepath.Base(filepath.Dir(filepath.Dir(executable))) != "versions" {
		installRoot = ""
	}
	if runtime.GOOS == "windows" {
		if installRoot == "" {
			installRoot = filepath.Join(os.Getenv("LOCALAPPDATA"), "Mira")
		}
		installer := filepath.Join(installRoot, "install.ps1")
		if _, err := os.Stat(installer); err != nil {
			return nil, fmt.Errorf("this copy was not installed by the Mira installer; install the release first")
		}
		command = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", installer, "-Update", "-Version", target, "-InstallDirectory", installRoot)
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		if installRoot == "" {
			installRoot = filepath.Join(home, ".local", "share", "mira")
		}
		installer := filepath.Join(installRoot, "install.sh")
		if _, err := os.Stat(installer); err != nil {
			return nil, fmt.Errorf("this copy was not installed by the Mira installer; install the release first")
		}
		arguments := []string{installer, "--update", "--version", target}
		if installRoot != filepath.Join(home, ".local", "share", "mira") {
			arguments = append(arguments, "--prefix", filepath.Dir(filepath.Dir(installRoot)))
		}
		command = exec.CommandContext(ctx, "sh", arguments...)
	}
	fmt.Fprintf(stdout, "Updating Mira %s -> %s. Node identity and configuration will be preserved.\n", Version, target)
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	return nil, command.Run()
}
