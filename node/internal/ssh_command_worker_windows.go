//go:build windows

package node

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

var getOEMCodePage = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetOEMCP")

func RunSSHCommandWorker(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) != 1 {
		return 255, fmt.Errorf("invalid Windows SSH text worker invocation")
	}
	encoded, err := base64.StdEncoding.DecodeString(args[0])
	if err != nil {
		return 255, fmt.Errorf("invalid Windows SSH command encoding")
	}
	codePage, _, _ := getOEMCodePage.Call()
	if codePage == 0 {
		return 255, fmt.Errorf("read Windows OEM code page")
	}
	decodeOEM := func(input []byte) ([]byte, error) {
		return decodeWindowsCodePage(uint32(codePage), input)
	}
	out := newSSHCommandTextWriter(stdout, decodeOEM)
	errout := newSSHCommandTextWriter(stderr, decodeOEM)
	shell := os.Getenv("COMSPEC")
	if shell == "" {
		shell = "cmd.exe"
	}
	// /u makes cmd.exe built-ins Unicode. chcp affects programs launched after
	// it, so modern child processes emit UTF-8 while the adapter also accepts
	// legacy OEM output from programs that ignore the active console page.
	command := backgroundCommand(exec.CommandContext(ctx, shell, "/d", "/u", "/s", "/c", "chcp.com 65001 >nul & "+string(encoded)))
	command.Stdin, command.Stdout, command.Stderr = stdin, out, errout
	runErr := command.Run()
	if closeErr := out.Close(); runErr == nil && closeErr != nil {
		runErr = closeErr
	}
	if closeErr := errout.Close(); runErr == nil && closeErr != nil {
		runErr = closeErr
	}
	if runErr == nil {
		return 0, nil
	}
	var exitError *exec.ExitError
	if errors.As(runErr, &exitError) {
		if code := exitError.ExitCode(); code >= 0 {
			return code, nil
		}
		return 255, nil
	}
	return 255, runErr
}

func decodeWindowsCodePage(codePage uint32, input []byte) ([]byte, error) {
	if len(input) == 0 {
		return nil, nil
	}
	const mbErrInvalidChars = 0x00000008
	needed, err := windows.MultiByteToWideChar(codePage, mbErrInvalidChars, &input[0], int32(len(input)), nil, 0)
	if err != nil {
		return nil, err
	}
	wide := make([]uint16, needed)
	if _, err = windows.MultiByteToWideChar(codePage, mbErrInvalidChars, &input[0], int32(len(input)), &wide[0], needed); err != nil {
		return nil, err
	}
	return []byte(string(utf16.Decode(wide))), nil
}
