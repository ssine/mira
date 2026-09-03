package node

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type config struct {
	ServerURL          string
	Token              string
	IdentityFile       string
	NodeKey            string
	AllowedRoots       []string
	HeartbeatInterval  time.Duration
	PrivilegeMode      string
	BridgeURL          string
	BridgeToken        string
	ExitWithParent     bool
	CodexBinary        string
	AppServerAutoStart bool
	AppServerListenURL string
	AppServerCodexHome string
	ConfigOverrides    []string
}

type fileConfig struct {
	ServerURL          string   `json:"serverUrl"`
	Token              string   `json:"token"`
	IdentityFile       string   `json:"identityFile"`
	LegacyStateFile    string   `json:"stateFile"`
	NodeKey            string   `json:"nodeKey"`
	AllowedRoots       []string `json:"allowedRoots"`
	HeartbeatSeconds   int64    `json:"heartbeatSeconds"`
	PrivilegeMode      string   `json:"privilegeMode"`
	BridgeURL          string   `json:"bridgeUrl"`
	BridgeToken        string   `json:"bridgeToken"`
	ExitWithParent     bool     `json:"exitWithParent"`
	CodexBinary        string   `json:"codexBinary"`
	AppServerAutoStart *bool    `json:"appServerAutoStart"`
	AppServerListenURL string   `json:"appServerListenUrl"`
	AppServerCodexHome string   `json:"appServerCodexHome"`
	ConfigOverrides    []string `json:"appServerConfigOverrides"`
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

func configFileFromArgs(args []string) (string, error) {
	path := firstEnv("MIRA_NODE_CONFIG_FILE", "ANDROID_NATIVE_CONFIG_FILE")
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--config":
			if index+1 >= len(args) || args[index+1] == "" {
				return "", fmt.Errorf("--config requires a path")
			}
			path = args[index+1]
			index++
		default:
			return "", fmt.Errorf("unknown argument: %s", args[index])
		}
	}
	return path, nil
}

func defaultAllowedRoots() []string {
	return platformDefaultFileRoots()
}

func parseStringArray(raw string, target *[]string, name string) error {
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("parse %s: %w", name, err)
	}
	return nil
}

func validateStringArray(values []string, maximum int, name string) error {
	if len(values) > maximum {
		return fmt.Errorf("%s must contain at most %d strings", name, maximum)
	}
	for _, value := range values {
		if value == "" || len(value) > 32768 || strings.ContainsRune(value, 0) {
			return fmt.Errorf("%s contains an invalid value", name)
		}
	}
	return nil
}

func loadConfigArgs(args []string) (config, error) {
	path, err := configFileFromArgs(args)
	if err != nil {
		return config{}, err
	}
	stored := fileConfig{}
	if path != "" {
		contents, err := os.ReadFile(path)
		if err != nil {
			return config{}, fmt.Errorf("read config file: %w", err)
		}
		if err := json.Unmarshal(contents, &stored); err != nil {
			return config{}, fmt.Errorf("parse config file: %w", err)
		}
	}

	roots := append([]string(nil), stored.AllowedRoots...)
	if len(roots) == 0 {
		roots = defaultAllowedRoots()
	}
	if err := parseStringArray(firstEnv("MIRA_NODE_ALLOWED_ROOTS", "NODE_AGENT_ALLOWED_ROOTS", "ANDROID_NATIVE_ALLOWED_ROOTS"), &roots, "MIRA_NODE_ALLOWED_ROOTS"); err != nil {
		return config{}, err
	}
	if len(roots) == 0 || len(roots) > 32 {
		return config{}, fmt.Errorf("MIRA_NODE_ALLOWED_ROOTS must contain 1 to 32 paths")
	}
	for index, root := range roots {
		if root == "" || !filepath.IsAbs(root) || strings.ContainsRune(root, 0) {
			return config{}, fmt.Errorf("allowed roots must be absolute paths")
		}
		roots[index] = filepath.Clean(root)
	}

	heartbeatSeconds := stored.HeartbeatSeconds
	if heartbeatSeconds == 0 {
		heartbeatSeconds = 3
	}
	if raw := firstEnv("MIRA_NODE_HEARTBEAT_SECONDS", "NODE_AGENT_HEARTBEAT_SECONDS"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > 300 {
			return config{}, fmt.Errorf("MIRA_NODE_HEARTBEAT_SECONDS must be between 1 and 300")
		}
		heartbeatSeconds = parsed
	}
	if heartbeatSeconds < 1 || heartbeatSeconds > 300 {
		return config{}, fmt.Errorf("heartbeatSeconds must be between 1 and 300")
	}

	serverURL := strings.TrimRight(stored.ServerURL, "/")
	if value := firstEnv("MIRA_SERVER_URL", "CONTROL_SERVER_URL"); value != "" {
		serverURL = strings.TrimRight(value, "/")
	}
	if serverURL == "" {
		serverURL = "http://127.0.0.1:8787"
	}
	parsedServerURL, err := url.Parse(serverURL)
	if err != nil || (parsedServerURL.Scheme != "http" && parsedServerURL.Scheme != "https") || parsedServerURL.Host == "" {
		return config{}, fmt.Errorf("serverUrl must be an absolute HTTP(S) URL")
	}
	// Explicit environment injection is retained only for development and
	// migration. Normal deployments read the shared protected identity file.
	token := firstEnv("MIRA_NODE_TOKEN", "CONTROL_SERVER_TOKEN")
	identityFile := stored.IdentityFile
	if identityFile == "" {
		identityFile = stored.LegacyStateFile
	}
	if value := firstEnv("MIRA_IDENTITY_FILE", "MIRA_NODE_STATE_FILE"); value != "" {
		identityFile = value
	}
	if identityFile == "" {
		identityFile, err = DefaultIdentityFile()
		if err != nil {
			return config{}, err
		}
	}
	if !filepath.IsAbs(identityFile) {
		return config{}, fmt.Errorf("identityFile must be an absolute path")
	}
	nodeKey := stored.NodeKey
	if value := firstEnv("MIRA_NODE_KEY", "NODE_AGENT_KEY"); value != "" {
		nodeKey = value
	}
	privilegeMode := stored.PrivilegeMode
	if value := firstEnv("MIRA_NODE_PRIVILEGE_MODE", "ANDROID_NATIVE_PRIVILEGE_MODE"); value != "" {
		privilegeMode = value
	}
	if privilegeMode == "" {
		privilegeMode = "auto"
	}
	if privilegeMode != "auto" && privilegeMode != "root" && privilegeMode != "app" {
		return config{}, fmt.Errorf("privilegeMode must be auto, root, or app")
	}
	bridgeURL := strings.TrimRight(stored.BridgeURL, "/")
	if value := firstEnv("MIRA_NODE_ANDROID_BRIDGE_URL", "ANDROID_NATIVE_BRIDGE_URL"); value != "" {
		bridgeURL = strings.TrimRight(value, "/")
	}
	bridgeToken := stored.BridgeToken
	if value := firstEnv("MIRA_NODE_ANDROID_BRIDGE_TOKEN", "ANDROID_NATIVE_BRIDGE_TOKEN"); value != "" {
		bridgeToken = value
	}
	if bridgeURL != "" {
		parsed, err := url.Parse(bridgeURL)
		if err != nil || parsed.Scheme != "http" || (parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
			return config{}, fmt.Errorf("bridgeUrl must be an HTTP loopback URL")
		}
		if bridgeToken == "" {
			return config{}, fmt.Errorf("bridgeToken is required when bridgeUrl is set")
		}
	}
	exitWithParent := stored.ExitWithParent
	if value := firstEnv("MIRA_NODE_EXIT_WITH_PARENT", "ANDROID_NATIVE_EXIT_WITH_PARENT"); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return config{}, fmt.Errorf("MIRA_NODE_EXIT_WITH_PARENT must be true or false")
		}
		exitWithParent = parsed
	}

	codexBinary := stored.CodexBinary
	if value := os.Getenv("CODEX_BINARY"); value != "" {
		codexBinary = value
	}
	autoStart := runtime.GOOS != "android"
	if stored.AppServerAutoStart != nil {
		autoStart = *stored.AppServerAutoStart
	}
	if value := os.Getenv("APP_SERVER_AUTO_START"); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return config{}, fmt.Errorf("APP_SERVER_AUTO_START must be true or false")
		}
		autoStart = parsed
	}
	listenURL := stored.AppServerListenURL
	if value := os.Getenv("APP_SERVER_LISTEN_URL"); value != "" {
		listenURL = value
	}
	if listenURL == "" {
		listenURL = "ws://127.0.0.1:4510"
	}
	codexHome := stored.AppServerCodexHome
	if value := os.Getenv("APP_SERVER_CODEX_HOME"); value != "" {
		codexHome = value
	}
	overrides := append([]string(nil), stored.ConfigOverrides...)
	if err := parseStringArray(os.Getenv("APP_SERVER_CONFIG_OVERRIDES"), &overrides, "APP_SERVER_CONFIG_OVERRIDES"); err != nil {
		return config{}, err
	}
	if err := validateStringArray(overrides, 20, "APP_SERVER_CONFIG_OVERRIDES"); err != nil {
		return config{}, err
	}
	for _, override := range overrides {
		lower := strings.ToLower(override)
		if strings.Contains(lower, "bearer_token") || strings.Contains(lower, "access_token") ||
			strings.Contains(lower, "password") || strings.Contains(lower, "api_key") ||
			strings.Contains(lower, "secret") {
			return config{}, fmt.Errorf("APP_SERVER_CONFIG_OVERRIDES must not contain credentials; use the Mira identity environment injection")
		}
	}

	return config{
		ServerURL: serverURL, Token: token, IdentityFile: identityFile, NodeKey: nodeKey, AllowedRoots: roots,
		HeartbeatInterval: time.Duration(heartbeatSeconds) * time.Second,
		PrivilegeMode:     privilegeMode, BridgeURL: bridgeURL, BridgeToken: bridgeToken,
		ExitWithParent: exitWithParent, CodexBinary: codexBinary,
		AppServerAutoStart: autoStart, AppServerListenURL: listenURL,
		AppServerCodexHome: codexHome, ConfigOverrides: overrides,
	}, nil
}

func loadConfig() (config, error) {
	return loadConfigArgs(os.Args[1:])
}
