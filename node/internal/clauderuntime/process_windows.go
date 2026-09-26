package clauderuntime

import (
	"os/exec"
	"strconv"
	"syscall"
)

func configureCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000}
}
func killCommand(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	killer := exec.Command("taskkill.exe", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
	configureCommand(killer)
	if killer.Run() != nil {
		_ = cmd.Process.Kill()
	}
}
