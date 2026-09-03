package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	ServerURL        string
	Token            string
	NodeKey          string
	AllowedRoots     []string
	HeartbeatSeconds time.Duration
	PrivilegeMode    string
	BridgeURL        string
	BridgeToken      string
	ExitWithParent   bool
}

type fileConfig struct {
	ServerURL        string   `json:"serverUrl"`
	Token            string   `json:"token"`
	NodeKey          string   `json:"nodeKey"`
	AllowedRoots     []string `json:"allowedRoots"`
	HeartbeatSeconds int64    `json:"heartbeatSeconds"`
	PrivilegeMode    string   `json:"privilegeMode"`
	BridgeURL        string   `json:"bridgeUrl"`
	BridgeToken      string   `json:"bridgeToken"`
	ExitWithParent   bool     `json:"exitWithParent"`
}

func configFileFromArgs(args []string) (string, error) {
	path := os.Getenv("ANDROID_NATIVE_CONFIG_FILE")
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

	roots := stored.AllowedRoots
	if len(roots) == 0 {
		roots = []string{"/sdcard", "/data/local/tmp"}
	}
	if encoded := os.Getenv("ANDROID_NATIVE_ALLOWED_ROOTS"); encoded != "" {
		if err := json.Unmarshal([]byte(encoded), &roots); err != nil {
			return config{}, fmt.Errorf("parse ANDROID_NATIVE_ALLOWED_ROOTS: %w", err)
		}
	}
	if len(roots) == 0 || len(roots) > 32 {
		return config{}, fmt.Errorf("ANDROID_NATIVE_ALLOWED_ROOTS must contain 1 to 32 paths")
	}
	for _, root := range roots {
		if root == "" || !strings.HasPrefix(root, "/") || strings.ContainsRune(root, 0) {
			return config{}, fmt.Errorf("Android allowed roots must be absolute paths")
		}
	}

	heartbeatSeconds := stored.HeartbeatSeconds
	if heartbeatSeconds == 0 {
		heartbeatSeconds = 3
	}
	if raw := os.Getenv("NODE_AGENT_HEARTBEAT_SECONDS"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > 300 {
			return config{}, fmt.Errorf("NODE_AGENT_HEARTBEAT_SECONDS must be between 1 and 300")
		}
		heartbeatSeconds = parsed
	}
	if heartbeatSeconds < 1 || heartbeatSeconds > 300 {
		return config{}, fmt.Errorf("heartbeatSeconds must be between 1 and 300")
	}

	serverURL := strings.TrimRight(stored.ServerURL, "/")
	if value := os.Getenv("CONTROL_SERVER_URL"); value != "" {
		serverURL = strings.TrimRight(value, "/")
	}
	if serverURL == "" {
		serverURL = "http://127.0.0.1:8787"
	}
	token := stored.Token
	if value := os.Getenv("CONTROL_SERVER_TOKEN"); value != "" {
		token = value
	}
	if token == "" {
		token = "local-poc-token"
	}
	nodeKey := stored.NodeKey
	if value := os.Getenv("NODE_AGENT_KEY"); value != "" {
		nodeKey = value
	}
	privilegeMode := stored.PrivilegeMode
	if value := os.Getenv("ANDROID_NATIVE_PRIVILEGE_MODE"); value != "" {
		privilegeMode = value
	}
	if privilegeMode == "" {
		privilegeMode = "auto"
	}
	if privilegeMode != "auto" && privilegeMode != "root" && privilegeMode != "app" {
		return config{}, fmt.Errorf("privilegeMode must be auto, root, or app")
	}
	bridgeURL := strings.TrimRight(stored.BridgeURL, "/")
	if value := os.Getenv("ANDROID_NATIVE_BRIDGE_URL"); value != "" {
		bridgeURL = strings.TrimRight(value, "/")
	}
	bridgeToken := stored.BridgeToken
	if value := os.Getenv("ANDROID_NATIVE_BRIDGE_TOKEN"); value != "" {
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
	if value := os.Getenv("ANDROID_NATIVE_EXIT_WITH_PARENT"); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return config{}, fmt.Errorf("ANDROID_NATIVE_EXIT_WITH_PARENT must be true or false")
		}
		exitWithParent = parsed
	}

	return config{
		ServerURL:        serverURL,
		Token:            token,
		NodeKey:          nodeKey,
		AllowedRoots:     roots,
		HeartbeatSeconds: time.Duration(heartbeatSeconds) * time.Second,
		PrivilegeMode:    privilegeMode,
		BridgeURL:        bridgeURL,
		BridgeToken:      bridgeToken,
		ExitWithParent:   exitWithParent,
	}, nil
}

func loadConfig() (config, error) {
	return loadConfigArgs(os.Args[1:])
}
