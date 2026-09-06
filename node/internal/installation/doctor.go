package installation

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func Doctor(ctx context.Context, stateDir string, dependencies Dependencies) DoctorReport {
	dependencies = withDefaults(dependencies)
	state, err := LoadState(dependencies.Files, stateDir)
	if err != nil {
		return DoctorReport{Findings: []Finding{{Code: "install_state_unreadable", Message: err.Error()}}}
	}
	report := DoctorReport{State: state}
	marker, err := dependencies.Files.ReadFile(filepath.Join(stateDir, "service-owner"))
	if err != nil || ServiceOwner(strings.TrimSpace(string(marker))) != state.ServiceOwner {
		report.Findings = append(report.Findings, Finding{Code: "service_owner_drift", Message: "install state and Supervisor service owner do not match"})
	}
	current, err := readCurrentVersion(dependencies.Files, stateDir, state.Platform)
	if err != nil {
		report.Findings = append(report.Findings, Finding{Code: "current_unreadable", Message: err.Error()})
	} else {
		report.CurrentVersion = current
		if current != state.Version {
			report.Findings = append(report.Findings, Finding{Code: "version_drift", Message: fmt.Sprintf("install state records %s but current selects %s", state.Version, current)})
		}
	}
	definition, err := currentServiceDefinition(ctx, dependencies, state)
	if err != nil {
		report.Findings = append(report.Findings, Finding{Code: "service_definition_unreadable", Message: err.Error()})
	} else {
		digest := sha256.Sum256([]byte(definition))
		if hex.EncodeToString(digest[:]) != state.ServiceDefinitionSHA256 {
			report.Findings = append(report.Findings, Finding{Code: "service_definition_drift", Message: "installed service definition differs from install state"})
		}
	}
	if state.Platform == "windows" {
		status, statusErr := readWindowsServiceStatus(ctx, dependencies, state.ServiceName)
		if statusErr != nil {
			report.Findings = append(report.Findings, Finding{Code: "service_status_unreadable", Message: statusErr.Error()})
		} else {
			if !strings.EqualFold(status.State, "Running") {
				report.Findings = append(report.Findings, Finding{Code: "service_inactive", Message: "Mira Supervisor Windows service is not running"})
			}
			if !strings.EqualFold(status.StartMode, "Auto") {
				report.Findings = append(report.Findings, Finding{Code: "service_start_mode_drift", Message: "Mira Supervisor Windows service is not automatic"})
			}
			if status.FailureFlag != 1 || !validWindowsFailureActions(status.FailureActions) {
				report.Findings = append(report.Findings, Finding{Code: "service_recovery_drift", Message: "Windows service recovery no longer restarts the Mira Supervisor after handoff or failure"})
			}
		}
	}
	report.Healthy = len(report.Findings) == 0
	return report
}

type windowsServiceStatus struct {
	State          string `json:"state"`
	StartMode      string `json:"startMode"`
	FailureFlag    int    `json:"failureFlag"`
	FailureActions string `json:"failureActions"`
}

func readWindowsServiceStatus(ctx context.Context, dependencies Dependencies, serviceName string) (windowsServiceStatus, error) {
	command := `$service=Get-CimInstance Win32_Service -Filter "Name='` + serviceName + `'"; ` +
		`$registry=Get-ItemProperty -LiteralPath 'HKLM:\SYSTEM\CurrentControlSet\Services\` + serviceName + `'; ` +
		`[pscustomobject]@{state=$service.State;startMode=$service.StartMode;failureFlag=$registry.FailureActionsOnNonCrashFailures;failureActions=[Convert]::ToBase64String([byte[]]$registry.FailureActions)} | ConvertTo-Json -Compress`
	output, err := dependencies.Runner.Run(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", command)
	if err != nil {
		return windowsServiceStatus{}, err
	}
	var status windowsServiceStatus
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &status); err != nil {
		return windowsServiceStatus{}, fmt.Errorf("decode Windows service status: %w", err)
	}
	return status, nil
}

func validWindowsFailureActions(encoded string) bool {
	value, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(value) < 20 {
		return false
	}
	if binary.LittleEndian.Uint32(value[0:4]) != 86400 || binary.LittleEndian.Uint32(value[12:16]) != 3 {
		return false
	}
	offset := int(binary.LittleEndian.Uint32(value[16:20]))
	if offset < 20 || offset+24 > len(value) {
		return false
	}
	for index := 0; index < 3; index++ {
		action := value[offset+index*8 : offset+(index+1)*8]
		if binary.LittleEndian.Uint32(action[0:4]) != 1 || binary.LittleEndian.Uint32(action[4:8]) != 3000 {
			return false
		}
	}
	return true
}

func readCurrentVersion(files FileSystem, stateDir, platform string) (string, error) {
	path := filepath.Join(stateDir, "current")
	if platform == "windows" {
		content, err := files.ReadFile(path)
		if err != nil {
			return "", err
		}
		version := strings.TrimSpace(string(content))
		if !versionPattern.MatchString(version) {
			return "", fmt.Errorf("invalid current Mira version")
		}
		return version, nil
	}
	target, err := files.Readlink(path)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(target) {
		return "", fmt.Errorf("current Mira version link is not relative")
	}
	resolved := filepath.Clean(filepath.Join(stateDir, target))
	versions := filepath.Join(stateDir, "versions")
	if !pathInside(versions, resolved) {
		return "", fmt.Errorf("current Mira version link escapes the versions directory")
	}
	version := filepath.Base(resolved)
	if filepath.Dir(resolved) != versions || !versionPattern.MatchString(version) {
		return "", fmt.Errorf("invalid current Mira version link")
	}
	return version, nil
}

func currentServiceDefinition(ctx context.Context, dependencies Dependencies, state InstallState) (string, error) {
	if state.Platform == "linux" {
		if state.ServiceOwner == ServiceOwnerMira {
			arguments := []string{}
			if state.ServiceScope == ScopeUser {
				arguments = append(arguments, "--user")
			}
			fragment, err := dependencies.Runner.Run(ctx, "systemctl", append(arguments, "show", state.ServiceName, "--property=FragmentPath", "--value")...)
			if err != nil {
				return "", err
			}
			fragment = strings.TrimSpace(fragment)
			if fragment != "" && filepath.Clean(fragment) != filepath.Clean(state.ServicePath) {
				return "", fmt.Errorf("%w: systemd loads %s from %s instead of %s", ErrOwnershipConflict, state.ServiceName, fragment, state.ServicePath)
			}
			dropIns, err := dependencies.Runner.Run(ctx, "systemctl", append(arguments, "show", state.ServiceName, "--property=DropInPaths", "--value")...)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(dropIns) != "" {
				return "", fmt.Errorf("%w: systemd drop-ins modify %s", ErrOwnershipConflict, state.ServiceName)
			}
		}
		content, err := dependencies.Files.ReadFile(state.ServicePath)
		return string(content), err
	}
	output, err := dependencies.Runner.Run(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "(Get-CimInstance Win32_Service -Filter \"Name='"+state.ServiceName+"'\").PathName")
	if err != nil {
		return "", err
	}
	definition := strings.TrimSpace(output)
	if definition == "" {
		return "", os.ErrNotExist
	}
	return definition, nil
}
