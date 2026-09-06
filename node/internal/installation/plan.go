package installation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ssine/mira/node/internal/supervisor"
)

var versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var serviceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]*$`)

const (
	RoleNode    = "node"
	RoleServer  = "server"
	ScopeUser   = "user"
	ScopeSystem = "system"
)

func DetectNixOS(files FileSystem) (bool, error) {
	if files == nil {
		files = OSFileSystem{}
	}
	content, err := files.ReadFile("/etc/os-release")
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, line := range strings.Split(string(content), "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key == "ID" && strings.EqualFold(value, "nixos") {
			return true, nil
		}
		if key == "ID_LIKE" {
			for _, item := range strings.Fields(value) {
				if strings.EqualFold(item, "nixos") {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func BuildPlan(options PlanOptions, files FileSystem) (InstallPlan, error) {
	if files == nil {
		files = OSFileSystem{}
	}
	layout, err := supervisor.NewLayout(options.StateDir)
	if err != nil {
		return InstallPlan{}, err
	}
	if !versionPattern.MatchString(options.Version) || options.Version == "." || options.Version == ".." {
		return InstallPlan{}, fmt.Errorf("invalid Mira version %q", options.Version)
	}
	platform := options.Platform
	if platform == "" {
		platform = runtime.GOOS
	}
	if platform != "linux" && platform != "windows" {
		return InstallPlan{}, fmt.Errorf("unsupported service platform %q", platform)
	}
	nixOS := false
	if platform == "linux" {
		nixOS, err = DetectNixOS(files)
		if err != nil {
			return InstallPlan{}, fmt.Errorf("detect NixOS: %w", err)
		}
	}
	owner := options.ServiceOwner
	if owner == "" {
		if nixOS {
			return InstallPlan{}, fmt.Errorf("%w: choose --service-owner nix or --service-owner mira", ErrOwnerChoiceNeeded)
		} else {
			owner = ServiceOwnerMira
		}
	}
	if owner != ServiceOwnerNix && owner != ServiceOwnerMira {
		return InstallPlan{}, fmt.Errorf("service owner must be %q or %q", ServiceOwnerNix, ServiceOwnerMira)
	}
	if owner == ServiceOwnerNix && (platform != "linux" || !nixOS) {
		return InstallPlan{}, fmt.Errorf("Nix service ownership requires a detected NixOS host")
	}
	role := options.Role
	if role == "" {
		role = RoleNode
	}
	if role != RoleNode && role != RoleServer {
		return InstallPlan{}, fmt.Errorf("Mira role must be %q or %q", RoleNode, RoleServer)
	}
	manager := options.ServiceManager
	if platform == "linux" {
		if owner == ServiceOwnerNix {
			if manager != "" && manager != ServiceManagerAuto && manager != ServiceManagerSystemd {
				return InstallPlan{}, fmt.Errorf("Nix service ownership requires the systemd service manager")
			}
			manager = ServiceManagerSystemd
		} else if manager == "" || manager == ServiceManagerAuto {
			manager, err = DetectLinuxServiceManager(files)
			if err != nil {
				return InstallPlan{}, err
			}
		} else if manager != ServiceManagerSystemd && manager != ServiceManagerProcd {
			return InstallPlan{}, fmt.Errorf("Linux service manager must be %q or %q", ServiceManagerSystemd, ServiceManagerProcd)
		}
		if manager == ServiceManagerProcd {
			if owner != ServiceOwnerMira {
				return InstallPlan{}, fmt.Errorf("procd services require Mira service ownership")
			}
			if role != RoleNode {
				return InstallPlan{}, fmt.Errorf("procd service installation currently supports only the Mira Node role")
			}
			if err := validateProcdHost(files); err != nil {
				return InstallPlan{}, err
			}
		}
	} else if manager != "" && manager != ServiceManagerAuto {
		return InstallPlan{}, fmt.Errorf("--service-manager is only supported on Linux")
	} else {
		manager = ""
	}
	scope := options.ServiceScope
	if scope == "" {
		if platform == "linux" && manager == ServiceManagerProcd {
			scope = ScopeSystem
		} else if platform == "linux" && owner == ServiceOwnerMira && role == RoleNode {
			scope = ScopeUser
		} else {
			scope = ScopeSystem
		}
	}
	if scope != ScopeUser && scope != ScopeSystem {
		return InstallPlan{}, fmt.Errorf("service scope must be %q or %q", ScopeUser, ScopeSystem)
	}
	if (owner == ServiceOwnerNix || platform == "windows") && scope != ScopeSystem {
		return InstallPlan{}, fmt.Errorf("%s-owned %s services require system scope", owner, platform)
	}
	if manager == ServiceManagerProcd && scope != ScopeSystem {
		return InstallPlan{}, fmt.Errorf("procd services require system scope")
	}

	plan := InstallPlan{
		StateDir:      layout.StateDir,
		StatePath:     filepath.Join(layout.StateDir, "install-state.json"),
		OwnerPath:     filepath.Join(layout.StateDir, "service-owner"),
		DetectedNixOS: nixOS,
	}
	state := InstallState{
		SchemaVersion:  installStateSchema,
		ServiceOwner:   owner,
		ServiceManager: manager,
		Role:           role,
		ServiceScope:   scope,
		Platform:       platform,
		Version:        options.Version,
	}
	if owner == ServiceOwnerNix {
		path := options.NixSnippetPath
		if path == "" {
			path = filepath.Join(layout.StateDir, "mira-service.nix")
		}
		if !safeAbsolutePath(path) || !pathInside(layout.StateDir, path) {
			return InstallPlan{}, fmt.Errorf("Nix snippet path must stay inside the Mira state directory")
		}
		definition, err := nixModule(layout.StateDir, role)
		if err != nil {
			return InstallPlan{}, err
		}
		state.ServiceName = "mira"
		state.ServicePath = filepath.Clean(path)
		state.ServiceDefinition = definition
		plan.NixSnippet = definition
		plan.Files = []PlannedFile{{Path: state.ServicePath, Content: []byte(definition), Mode: 0644, DirMode: 0700}}
	} else if platform == "linux" && manager == ServiceManagerSystemd {
		path := options.SystemdUnitPath
		if path == "" {
			if scope == ScopeUser {
				home, err := os.UserHomeDir()
				if err != nil {
					return InstallPlan{}, err
				}
				path = filepath.Join(home, ".config", "systemd", "user", "mira.service")
			} else {
				path = "/etc/systemd/system/mira.service"
			}
		}
		if !safeAbsolutePath(path) {
			return InstallPlan{}, fmt.Errorf("systemd unit path must be an absolute file path")
		}
		definition, err := systemdUnit(layout.StateDir, role, scope)
		if err != nil {
			return InstallPlan{}, err
		}
		state.ServiceName = filepath.Base(path)
		if !serviceNamePattern.MatchString(strings.TrimSuffix(state.ServiceName, ".service")) || !strings.HasSuffix(state.ServiceName, ".service") {
			return InstallPlan{}, fmt.Errorf("invalid systemd service filename %q", state.ServiceName)
		}
		state.ServicePath = filepath.Clean(path)
		state.ServiceDefinition = definition
		plan.Files = []PlannedFile{{Path: state.ServicePath, Content: []byte(definition), Mode: 0644, DirMode: 0755}}
		managerArgs := []string{}
		if scope == ScopeUser {
			managerArgs = append(managerArgs, "--user")
		}
		plan.Commands = []Command{
			{Name: "systemctl", Args: append(append([]string(nil), managerArgs...), "daemon-reload")},
			{Name: "systemctl", Args: append(append([]string(nil), managerArgs...), "enable", "--now", state.ServiceName)},
		}
	} else if platform == "linux" {
		path := options.ProcdInitPath
		if path == "" {
			path = defaultProcdInitPath
		}
		if !safeAbsolutePath(path) {
			return InstallPlan{}, fmt.Errorf("procd init path must be an absolute file path")
		}
		definition, err := procdInitScript(layout.StateDir, role)
		if err != nil {
			return InstallPlan{}, err
		}
		state.ServiceName = filepath.Base(path)
		if !serviceNamePattern.MatchString(state.ServiceName) {
			return InstallPlan{}, fmt.Errorf("invalid procd service filename %q", state.ServiceName)
		}
		state.ServicePath = filepath.Clean(path)
		state.ServiceDefinition = definition
		plan.Files = []PlannedFile{{Path: state.ServicePath, Content: []byte(definition), Mode: 0755, DirMode: 0755}}
		plan.Commands = procdCommands(state.ServicePath, "start")
	} else {
		serviceName := options.WindowsServiceName
		if serviceName == "" {
			serviceName = "Mira"
		}
		if !serviceNamePattern.MatchString(serviceName) {
			return InstallPlan{}, fmt.Errorf("invalid Windows service name")
		}
		executable := options.WindowsExecutable
		if executable == "" {
			directory, err := layout.VersionDir(options.Version)
			if err != nil {
				return InstallPlan{}, err
			}
			executable = filepath.Join(directory, "mira.exe")
		}
		if !safeAbsolutePath(executable) {
			return InstallPlan{}, fmt.Errorf("Windows executable must be absolute")
		}
		definition, err := windowsCommandLine(executable, layout.StateDir, role, serviceName)
		if err != nil {
			return InstallPlan{}, err
		}
		state.ServiceName = serviceName
		state.ServiceDefinition = definition
		plan.Commands = []Command{
			{Name: "sc.exe", Args: []string{"create", serviceName, "binPath=", definition, "start=", "auto", "DisplayName=", "Mira Supervisor"}},
		}
		plan.Commands = append(plan.Commands, windowsServicePolicyCommands(serviceName)...)
		plan.Commands = append(plan.Commands, Command{Name: "sc.exe", Args: []string{"start", serviceName}})
	}
	digest := sha256.Sum256([]byte(state.ServiceDefinition))
	state.ServiceDefinitionSHA256 = hex.EncodeToString(digest[:])
	plan.State = state
	return plan, nil
}

func windowsServicePolicyCommands(serviceName string) []Command {
	return []Command{
		{Name: "sc.exe", Args: []string{"failure", serviceName, "reset=", "86400", "actions=", "restart/3000/restart/3000/restart/3000"}},
		{Name: "sc.exe", Args: []string{"failureflag", serviceName, "1"}},
	}
}

func safeAbsolutePath(path string) bool {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") {
		return false
	}
	clean := filepath.Clean(path)
	return filepath.Dir(clean) != clean
}

func pathInside(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !filepath.IsAbs(relative) && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func systemdQuote(value string) (string, error) {
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("service path contains a control character")
	}
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`, nil
}

// systemdEnvironmentFilePath escapes an absolute path as one unquoted
// EnvironmentFile= argument. The optional-file prefix must remain outside the
// path: quoting "-<path>" makes some systemd versions treat the dash as part of
// the filename. Hex escapes avoid word splitting and quote parsing, while %%
// survives systemd's separate specifier expansion as one literal percent.
func systemdEnvironmentFilePath(value string) (string, error) {
	// BuildPlan is intentionally cross-platform testable. A Windows runner may
	// construct a Linux plan in a native temporary directory even though real
	// systemd installation only occurs on Linux.
	if !path.IsAbs(value) && !(runtime.GOOS == "windows" && filepath.IsAbs(value)) {
		return "", fmt.Errorf("systemd environment file path must be absolute")
	}
	if strings.IndexByte(value, 0) >= 0 {
		return "", fmt.Errorf("systemd environment file path contains NUL")
	}
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("systemd environment file path is not valid UTF-8")
	}

	var escaped strings.Builder
	escaped.Grow(len(value))
	for _, character := range value {
		switch {
		case character == '%':
			escaped.WriteString("%%")
		case character >= '!' && character <= '~' && character != '\\' && character != '\'' && character != '"':
			escaped.WriteRune(character)
		case character > unicode.MaxASCII && !unicode.IsControl(character):
			escaped.WriteRune(character)
		case character <= unicode.MaxASCII:
			fmt.Fprintf(&escaped, `\x%02x`, character)
		case character <= 0xffff:
			fmt.Fprintf(&escaped, `\u%04x`, character)
		default:
			fmt.Fprintf(&escaped, `\U%08x`, character)
		}
	}
	return escaped.String(), nil
}

func supervisorRoleArgument(role string) string {
	if role == RoleServer {
		return " --server"
	}
	return ""
}

func systemdUnit(stateDir, role, scope string) (string, error) {
	executable, err := systemdQuote(path.Join(stateDir, "current", "mira"))
	if err != nil {
		return "", err
	}
	state, err := systemdQuote(stateDir)
	if err != nil {
		return "", err
	}
	environmentFile, err := systemdEnvironmentFilePath(path.Join(stateDir, "mira.env"))
	if err != nil {
		return "", err
	}
	wantedBy := "multi-user.target"
	if scope == ScopeUser {
		wantedBy = "default.target"
	}
	service := []string{
		"# Managed by Mira installer",
		"[Unit]",
		"Description=Mira Supervisor",
		"After=network-online.target",
		"Wants=network-online.target",
		"",
		"[Service]",
		"Type=simple",
	}
	if scope == ScopeSystem {
		service = append(service, "Environment=HOME=/root")
	}
	service = append(service,
		"EnvironmentFile=-"+environmentFile,
		"ExecStart="+executable+" supervisor --state-dir "+state+" --service-owner mira"+supervisorRoleArgument(role),
		// A successful update deliberately exits the old Supervisor after the
		// current pointer is committed. The service manager then starts the new
		// Supervisor from that pointer; explicit systemctl stop is not restarted.
		"Restart=always",
		"RestartSec=3",
		"TimeoutStopSec=30",
		"KillMode=mixed",
		"",
		"[Install]",
		"WantedBy="+wantedBy,
		"",
	)
	return strings.Join(service, "\n"), nil
}

func nixString(value string) (string, error) {
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("Nix path contains a control character")
	}
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, `${`, `\${`)
	return `"` + value + `"`, nil
}

func nixModule(stateDir, role string) (string, error) {
	state, err := nixString(stateDir)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		"# Generated for review by `mira install`; import it from configuration.nix.",
		"# Mira never runs nixos-rebuild and never edits systemd in Nix ownership mode.",
		"{ ... }:",
		"let",
		"  miraStateDir = " + state + ";",
		"in",
		"{",
		"  systemd.services.mira = {",
		"    description = \"Mira Supervisor\";",
		"    wantedBy = [ \"multi-user.target\" ];",
		"    after = [ \"network-online.target\" ];",
		"    wants = [ \"network-online.target\" ];",
		"    environment.HOME = \"/root\";",
		"    serviceConfig = {",
		"      EnvironmentFile = [ \"-${miraStateDir}/mira.env\" ];",
		"      ExecStart = \"${miraStateDir}/current/mira supervisor --state-dir ${miraStateDir} --service-owner nix" + supervisorRoleArgument(role) + "\";",
		"      Restart = \"always\";",
		"      RestartSec = 3;",
		"      TimeoutStopSec = 30;",
		"      KillMode = \"mixed\";",
		"    };",
		"  };",
		"}",
		"",
	}, "\n"), nil
}

func windowsQuote(value string) (string, error) {
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("Windows service argument contains a control character")
	}
	// Service paths generated here are not arbitrary command lines. Rejecting a
	// literal quote keeps CreateProcess parsing unambiguous.
	if strings.Contains(value, `"`) {
		return "", fmt.Errorf("Windows service argument contains a quote")
	}
	return `"` + value + `"`, nil
}

func windowsCommandLine(executable, stateDir, role, serviceName string) (string, error) {
	binary, err := windowsQuote(executable)
	if err != nil {
		return "", err
	}
	state, err := windowsQuote(stateDir)
	if err != nil {
		return "", err
	}
	service, err := windowsQuote(serviceName)
	if err != nil {
		return "", err
	}
	return binary + " supervisor --state-dir " + state + " --service-owner mira --windows-service-name " + service + supervisorRoleArgument(role), nil
}
