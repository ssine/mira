//go:build windows

package node

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var impersonateLoggedOnUser = windows.NewLazySystemDLL("advapi32.dll").NewProc("ImpersonateLoggedOnUser")

type windowsSessionIdentity struct {
	sessionID uint32
	identity  string
	token     windows.Token
}

func windowsTokenIdentity(token windows.Token) (string, bool, error) {
	user, err := token.GetTokenUser()
	if err != nil {
		return "", false, err
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return "", false, err
	}
	isSystem := user.User.Sid.Equals(systemSID)
	account, domain, _, lookupErr := user.User.Sid.LookupAccount("")
	if lookupErr != nil {
		return user.User.Sid.String(), isSystem, nil
	}
	if domain != "" {
		return domain + `\` + account, isSystem, nil
	}
	return account, isSystem, nil
}

func currentWindowsSessionID() (uint32, error) {
	var sessionID uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &sessionID); err != nil {
		return 0, err
	}
	return sessionID, nil
}

func activeWindowsSessionIDs() ([]uint32, error) {
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessions, &count); err != nil {
		return nil, err
	}
	if sessions != nil {
		defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))
	}
	values := unsafe.Slice(sessions, count)
	ids := make([]uint32, 0, len(values))
	for _, session := range values {
		if session.State == windows.WTSActive {
			ids = append(ids, session.SessionID)
		}
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	return ids, nil
}

func queryWindowsSessionIdentity(sessionID uint32) (*windowsSessionIdentity, error) {
	var token windows.Token
	if err := windows.WTSQueryUserToken(sessionID, &token); err != nil {
		return nil, err
	}
	identity, isSystem, err := windowsTokenIdentity(token)
	if err != nil {
		_ = token.Close()
		return nil, err
	}
	if isSystem {
		_ = token.Close()
		return nil, fmt.Errorf("Windows session %d does not contain an interactive user", sessionID)
	}
	return &windowsSessionIdentity{sessionID: sessionID, identity: identity, token: token}, nil
}

func selectWindowsUserSession(requested *uint32) (*windowsSessionIdentity, error) {
	if requested != nil {
		identity, err := queryWindowsSessionIdentity(*requested)
		if err != nil {
			return nil, fmt.Errorf("open Windows user session %d: %w", *requested, err)
		}
		return identity, nil
	}
	ids, err := activeWindowsSessionIDs()
	if err != nil {
		return nil, fmt.Errorf("list active Windows user sessions: %w", err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no active interactive Windows user session; use executionContext system explicitly or log in a user")
	}
	console := windows.WTSGetActiveConsoleSessionId()
	for _, sessionID := range ids {
		if sessionID == console {
			return selectWindowsUserSession(&sessionID)
		}
	}
	if len(ids) > 1 {
		return nil, fmt.Errorf("multiple active Windows user sessions %v; set userSessionId explicitly", ids)
	}
	return selectWindowsUserSession(&ids[0])
}

func acquireExecutionIdentity(request executionRequest) (*executionIdentity, error) {
	if err := validateExecutionRequest(request); err != nil {
		return nil, err
	}
	currentToken := windows.GetCurrentProcessToken()
	currentIdentity, currentIsSystem, err := windowsTokenIdentity(currentToken)
	if err != nil {
		return nil, fmt.Errorf("read Windows Node identity: %w", err)
	}
	contextName := request.Context
	if contextName == "" {
		contextName = executionContextUser
	}
	if contextName == executionContextSystem {
		if !currentIsSystem {
			return nil, fmt.Errorf("system execution context is unavailable because this Windows Node is not running as LocalSystem")
		}
		return &executionIdentity{Context: executionContextSystem, OSIdentity: currentIdentity}, nil
	}
	if !currentIsSystem {
		sessionID, sessionErr := currentWindowsSessionID()
		if sessionErr != nil {
			return nil, fmt.Errorf("read current Windows user session: %w", sessionErr)
		}
		if request.UserSessionID != nil && *request.UserSessionID != sessionID {
			return nil, fmt.Errorf("userSessionId %d is unavailable from user-run Windows Node session %d", *request.UserSessionID, sessionID)
		}
		return &executionIdentity{Context: executionContextUser, OSIdentity: currentIdentity, UserSessionID: &sessionID}, nil
	}
	session, err := selectWindowsUserSession(request.UserSessionID)
	if err != nil {
		return nil, err
	}
	environment, err := session.token.Environ(false)
	if err != nil {
		_ = session.token.Close()
		return nil, fmt.Errorf("read Windows user environment for session %d: %w", session.sessionID, err)
	}
	// Managed processes capture stdout/stderr through inherited handles. Windows
	// forbids handle inheritance across Terminal Services sessions, so keep the
	// interactive user's logon identity and credentials but bind a duplicate
	// primary token to the Node service's non-interactive session.
	processSessionID, err := currentWindowsSessionID()
	if err != nil {
		_ = session.token.Close()
		return nil, fmt.Errorf("read Windows Node process session: %w", err)
	}
	if processSessionID != session.sessionID {
		var processToken windows.Token
		if err := windows.DuplicateTokenEx(
			session.token, windows.MAXIMUM_ALLOWED, nil,
			windows.SecurityImpersonation, windows.TokenPrimary, &processToken,
		); err != nil {
			_ = session.token.Close()
			return nil, fmt.Errorf("duplicate Windows user token for session %d: %w", session.sessionID, err)
		}
		if err := windows.SetTokenInformation(
			processToken, windows.TokenSessionId,
			(*byte)(unsafe.Pointer(&processSessionID)), uint32(unsafe.Sizeof(processSessionID)),
		); err != nil {
			_ = processToken.Close()
			_ = session.token.Close()
			return nil, fmt.Errorf("bind Windows user token to Node process session: %w", err)
		}
		_ = session.token.Close()
		session.token = processToken
	}
	return &executionIdentity{
		Context: executionContextUser, OSIdentity: session.identity, UserSessionID: &session.sessionID,
		token: uintptr(session.token), environment: environment, closeToken: true,
	}, nil
}

func (identity *executionIdentity) close() {
	if identity.closeToken && identity.token != 0 {
		_ = windows.Token(identity.token).Close()
		identity.token = 0
	}
}

func (identity *executionIdentity) runImpersonated(operation func() (any, error)) (result any, returnedErr error) {
	if identity.token == 0 {
		return operation()
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	resultCode, _, callErr := impersonateLoggedOnUser.Call(identity.token)
	if resultCode == 0 {
		return nil, fmt.Errorf("impersonate Windows user %s: %w", identity.OSIdentity, callErr)
	}
	defer func() {
		if err := windows.RevertToSelf(); returnedErr == nil && err != nil {
			result = nil
			returnedErr = fmt.Errorf("revert Windows user impersonation: %w", err)
		}
	}()
	return operation()
}

func windowsEnvironmentValue(environment []string, name string) string {
	for _, item := range environment {
		separator := strings.IndexByte(item, '=')
		if separator > 0 && strings.EqualFold(item[:separator], name) {
			return item[separator+1:]
		}
	}
	return ""
}

func windowsExecutableCandidates(name string, environment []string) []string {
	extensions := filepath.SplitList(windowsEnvironmentValue(environment, "PATHEXT"))
	if len(extensions) == 0 {
		extensions = []string{".COM", ".EXE", ".BAT", ".CMD"}
	}
	if filepath.Ext(name) != "" {
		return []string{name}
	}
	result := []string{name}
	for _, extension := range extensions {
		result = append(result, name+extension)
	}
	return result
}

func lookPathInWindowsEnvironment(name, cwd string, environment []string) (string, error) {
	hasSeparator := strings.ContainsAny(name, `/\`)
	if hasSeparator || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		base := name
		if !filepath.IsAbs(base) {
			base = filepath.Join(cwd, base)
		}
		for _, candidate := range windowsExecutableCandidates(base, environment) {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, nil
			}
		}
		return "", fmt.Errorf("executable file %s not found", name)
	}
	for _, directory := range filepath.SplitList(windowsEnvironmentValue(environment, "PATH")) {
		if directory == "" {
			continue
		}
		for _, candidate := range windowsExecutableCandidates(filepath.Join(directory, name), environment) {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("executable file %s not found in Windows user PATH", name)
}

func newExecutionCommand(identity *executionIdentity, name string, args []string, cwd string, overrides map[string]string) (*exec.Cmd, error) {
	environment := os.Environ()
	commandName := name
	if identity.token != 0 {
		environment = identity.environment
		resolved, err := lookPathInWindowsEnvironment(name, cwd, environment)
		if err != nil {
			return nil, err
		}
		commandName = resolved
	}
	command := backgroundCommand(exec.Command(commandName, args...))
	command.Dir = cwd
	command.Env = mergeExecutionEnvironment(environment, overrides, true)
	if identity.token != 0 {
		command.SysProcAttr.Token = syscall.Token(identity.token)
	}
	return command, nil
}

func executionContextStatus() map[string]any {
	currentToken := windows.GetCurrentProcessToken()
	currentIdentity, currentIsSystem, err := windowsTokenIdentity(currentToken)
	if err != nil {
		return map[string]any{"default": executionContextUser, "error": err.Error()}
	}
	if !currentIsSystem {
		sessionID, _ := currentWindowsSessionID()
		return map[string]any{
			"default": executionContextUser,
			"contexts": []map[string]any{
				{"name": executionContextUser, "available": true, "osIdentity": currentIdentity, "userSessionId": sessionID},
				{"name": executionContextSystem, "available": false},
			},
		}
	}
	contexts := []map[string]any{{"name": executionContextSystem, "available": true, "osIdentity": currentIdentity}}
	ids, listErr := activeWindowsSessionIDs()
	if listErr != nil {
		contexts = append(contexts, map[string]any{"name": executionContextUser, "available": false, "error": listErr.Error()})
	} else if len(ids) == 0 {
		contexts = append(contexts, map[string]any{"name": executionContextUser, "available": false})
	} else {
		for _, sessionID := range ids {
			session, sessionErr := queryWindowsSessionIdentity(sessionID)
			if sessionErr != nil {
				contexts = append(contexts, map[string]any{"name": executionContextUser, "available": false, "userSessionId": sessionID, "error": sessionErr.Error()})
				continue
			}
			contexts = append(contexts, map[string]any{"name": executionContextUser, "available": true, "osIdentity": session.identity, "userSessionId": sessionID})
			_ = session.token.Close()
		}
	}
	return map[string]any{"default": executionContextUser, "contexts": contexts}
}
