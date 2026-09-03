//go:build windows

package node

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"unsafe"

	pty "github.com/aymanbagabas/go-pty"
	"golang.org/x/sys/windows"
)

func configureSSHCommand(cmd *exec.Cmd) {}
func configureSSHPTY(cmd *pty.Cmd)      {}

func sshPTYReader(terminal pty.Pty) (io.Reader, func(), error) {
	// go-pty closes its output handle immediately after ClosePseudoConsole.
	// Keep a duplicate read handle until EOF so final buffered output cannot
	// race that close when a command exits before the reader is scheduled.
	console := terminal.(pty.ConPty)
	var handle windows.Handle
	if err := windows.DuplicateHandle(windows.CurrentProcess(), windows.Handle(console.OutputPipe().Fd()), windows.CurrentProcess(), &handle, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, nil, err
	}
	reader := os.NewFile(uintptr(handle), "ssh-conpty-output")
	if err := releaseSSHConsole(console); err != nil {
		reader.Close()
		return nil, nil, err
	}
	return reader, func() { reader.Close() }, nil
}

var releasePseudoConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReleasePseudoConsole")

func releaseSSHConsole(console pty.ConPty) error {
	// Release (unlike Close) lets conhost flush and exit when its last client
	// exits. Closing immediately after cmd.Wait can discard a pending frame.
	// https://learn.microsoft.com/windows/console/releasepseudoconsole
	if err := releasePseudoConsole.Find(); err == nil {
		hr, _, _ := releasePseudoConsole.Call(console.Fd())
		if hr != 0 {
			return fmt.Errorf("ReleasePseudoConsole: HRESULT 0x%x", hr)
		}
		return nil
	}
	// Microsoft's ConPTY maintainer recommends the old ABI shim only for OS
	// versions predating this API. Never interpret a future HPCON's layout.
	// https://github.com/microsoft/terminal/discussions/19112
	if windows.RtlGetVersion().BuildNumber >= 26100 {
		return fmt.Errorf("ReleasePseudoConsole unavailable on this Windows build")
	}
	// Legacy HPCON starts with hSignal, hPtyReference, hConPtyProcess.
	// Release only hPtyReference. Read/write our own OS allocation through the
	// Win32 APIs instead of converting a foreign uintptr to a Go pointer.
	// ABI: microsoft/terminal v1.18.3181.0 src/winconpty/winconpty.h
	var reference, zero windows.Handle
	size := unsafe.Sizeof(reference)
	address := console.Fd() + size
	if err := windows.ReadProcessMemory(windows.CurrentProcess(), address, (*byte)(unsafe.Pointer(&reference)), size, nil); err != nil {
		return err
	}
	if reference == 0 || reference == windows.InvalidHandle {
		return nil
	}
	if err := windows.WriteProcessMemory(windows.CurrentProcess(), address, (*byte)(unsafe.Pointer(&zero)), size, nil); err != nil {
		return err
	}
	return windows.CloseHandle(reference)
}

func signalSSHProcess(process *os.Process, name string) error {
	return terminateProcess(process, "SIG"+name)
}

// Closing the supervisor's job handle kills a worker's entire descendant tree,
// including on supervisor crash. Modern Windows supports nested jobs.
func guardSSHProcessTree(process *os.Process) (func(), error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(process.Pid))
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	defer windows.CloseHandle(handle)
	if err := windows.AssignProcessToJobObject(job, handle); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	return func() { windows.CloseHandle(job) }, nil
}
