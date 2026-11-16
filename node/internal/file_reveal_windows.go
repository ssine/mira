//go:build windows

package node

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

func desktopRevealAvailable() bool { return true }

// windowsEnvironmentBlock encodes NAME=value strings as the double-NUL
// terminated UTF-16 block CreateProcessAsUser expects.
func windowsEnvironmentBlock(environment []string) []uint16 {
	block := make([]uint16, 0, 4096)
	for _, item := range environment {
		block = append(block, utf16.Encode([]rune(item))...)
		block = append(block, 0)
	}
	if len(environment) == 0 {
		block = append(block, 0)
	}
	return append(block, 0)
}

// revealInFileManager starts Explorer for the interactive user. A LocalSystem
// service lives in the non-interactive session 0, so it launches Explorer with
// the selected session's own token on that session's desktop. This is the only
// place the Node starts a program on the interactive desktop: the program is
// fixed and its sole argument is an already authorized path.
func revealInFileManager(_ context.Context, request executionRequest, target string, directory bool) (string, error) {
	if request.Context == executionContextSystem {
		return "", fmt.Errorf("reveal needs an interactive Windows user session; executionContext system has no desktop")
	}
	root, err := windows.GetSystemWindowsDirectory()
	if err != nil {
		return "", fmt.Errorf("locate Windows Explorer: %w", err)
	}
	explorer := filepath.Join(root, "explorer.exe")
	// Explorer parses its own command line: `/select,"path"` selects a file in
	// its folder. Windows paths cannot contain quotes.
	commandLine := syscall.EscapeArg(explorer) + ` "` + target + `"`
	if !directory {
		commandLine = syscall.EscapeArg(explorer) + ` /select,"` + target + `"`
	}
	_, currentIsSystem, err := windowsTokenIdentity(windows.GetCurrentProcessToken())
	if err != nil {
		return "", fmt.Errorf("read Windows Node identity: %w", err)
	}
	if !currentIsSystem {
		command := exec.Command(explorer)
		command.SysProcAttr = &syscall.SysProcAttr{CmdLine: commandLine}
		if err := command.Start(); err != nil {
			return "", fmt.Errorf("start Windows Explorer: %w", err)
		}
		go reapFileManager(command)
		return target, nil
	}
	session, err := selectWindowsUserSession(request.UserSessionID)
	if err != nil {
		return "", err
	}
	defer session.token.Close()
	environment, err := session.token.Environ(false)
	if err != nil {
		return "", fmt.Errorf("read Windows user environment for session %d: %w", session.sessionID, err)
	}
	block := windowsEnvironmentBlock(environment)
	application, err := windows.UTF16PtrFromString(explorer)
	if err != nil {
		return "", err
	}
	command, err := windows.UTF16FromString(commandLine)
	if err != nil {
		return "", err
	}
	directoryPointer, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return "", err
	}
	desktop, err := windows.UTF16PtrFromString(`winsta0\default`)
	if err != nil {
		return "", err
	}
	startup := windows.StartupInfo{Desktop: desktop}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	var process windows.ProcessInformation
	if err := windows.CreateProcessAsUser(
		session.token, application, &command[0], nil, nil, false,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NEW_PROCESS_GROUP,
		&block[0], directoryPointer, &startup, &process,
	); err != nil {
		if err == windows.ERROR_ACCESS_DENIED {
			// A job object binds its processes to one session; the installed
			// service is in none, a Node started inside another tool's job is.
			return "", fmt.Errorf("start Windows Explorer in session %d: access denied (a Node running inside a job object cannot start a program in the interactive session)", session.sessionID)
		}
		return "", fmt.Errorf("start Windows Explorer in session %d: %w", session.sessionID, err)
	}
	_ = windows.CloseHandle(process.Thread)
	_ = windows.CloseHandle(process.Process)
	return target, nil
}
