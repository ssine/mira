package installation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultProcdInitPath = "/etc/init.d/mira"

// DetectLinuxServiceManager recognizes OpenWrt procd and the systemd host layout.
// BuildPlan additionally probes the selected scope before auto selects systemd.
func DetectLinuxServiceManager(files FileSystem) (ServiceManager, error) {
	if files == nil {
		files = OSFileSystem{}
	}
	openWrt, err := detectOpenWrt(files)
	if err != nil {
		return "", err
	}
	if openWrt && validateProcdHost(files) == nil {
		return ServiceManagerProcd, nil
	}
	if _, err := files.Stat("/run/systemd/system"); err == nil {
		return ServiceManagerSystemd, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return ServiceManagerBuiltin, nil
}

func detectOpenWrt(files FileSystem) (bool, error) {
	if _, err := files.Stat("/etc/openwrt_release"); err == nil {
		return true, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("detect OpenWrt: %w", err)
	}
	content, err := files.ReadFile("/etc/os-release")
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("detect OpenWrt: %w", err)
	}
	for _, line := range strings.Split(string(content), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found || (key != "ID" && key != "ID_LIKE") {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		for _, item := range strings.Fields(value) {
			if strings.EqualFold(item, "openwrt") || strings.EqualFold(item, "friendlywrt") {
				return true, nil
			}
		}
	}
	return false, nil
}

func validateProcdHost(files FileSystem) error {
	for _, path := range []string{"/etc/rc.common", "/sbin/procd"} {
		info, err := files.Stat(path)
		if err != nil {
			return fmt.Errorf("procd service installation requires %s: %w", path, err)
		}
		if info.IsDir() || info.Mode()&0111 == 0 {
			return fmt.Errorf("procd service installation requires executable %s", path)
		}
	}
	return nil
}

func procdQuote(value string) (string, error) {
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("procd service argument contains a control character")
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'", nil
}

func procdInitScript(stateDir, role string) (string, error) {
	if role != RoleNode {
		return "", fmt.Errorf("procd service installation currently supports only the Mira Node role")
	}
	executable, err := procdQuote(filepath.Join(stateDir, "current", "mira"))
	if err != nil {
		return "", err
	}
	state, err := procdQuote(stateDir)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		"#!/bin/sh /etc/rc.common",
		"# Managed by Mira installer",
		"",
		"START=99",
		"STOP=10",
		"USE_PROCD=1",
		"",
		"start_service() {",
		"\tprocd_open_instance",
		"\tprocd_set_param command " + executable + " supervisor --state-dir " + state + " --service-owner mira",
		"\tprocd_set_param env HOME=/root",
		"\tprocd_set_param respawn 3600 3 0",
		"\tprocd_set_param term_timeout 30",
		"\tprocd_set_param stdout 1",
		"\tprocd_set_param stderr 1",
		"\tprocd_close_instance",
		"}",
		"",
	}, "\n"), nil
}

func procdCommands(servicePath string, action string) []Command {
	commands := []Command{{Name: servicePath, Args: []string{"enable"}}}
	if action != "" {
		commands = append(commands, Command{Name: servicePath, Args: []string{action}})
	}
	return commands
}

func usableSystemd(options PlanOptions, role string) bool {
	scope := options.ServiceScope
	if scope == "" {
		scope = ScopeSystem
		if role == RoleNode {
			scope = ScopeUser
		}
	}
	args := []string{}
	if scope == ScopeUser {
		args = append(args, "--user")
	}
	args = append(args, "show", "--property=Version", "--value")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := withDefaults(Dependencies{Runner: options.ManagerProbe}).Runner
	output, err := runner.Run(ctx, "systemctl", args...)
	return err == nil && strings.TrimSpace(output) != ""
}
