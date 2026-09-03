//go:build windows

package node

import (
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
	return reader, func() { reader.Close() }, nil
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
