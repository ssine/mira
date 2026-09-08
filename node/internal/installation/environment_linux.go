package installation

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

func configureServiceEnvironment(command *exec.Cmd) {
	configureUserRuntimeDirectory(command, "/run/user", os.Geteuid())
}

// Some SSH sessions omit the logind environment even while the user's manager
// is running. Supply its conventional runtime directory only to systemctl;
// never change the parent environment or create a substitute runtime directory.
func configureUserRuntimeDirectory(command *exec.Cmd, root string, uid int) {
	if filepath.Base(command.Path) != "systemctl" || !slices.Contains(command.Args[1:], "--user") {
		return
	}
	environment := command.Environ()
	for _, entry := range environment {
		if strings.HasPrefix(entry, "XDG_RUNTIME_DIR=") && entry != "XDG_RUNTIME_DIR=" {
			return
		}
	}
	directory := filepath.Join(root, strconv.Itoa(uid))
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(uid) {
		return
	}
	command.Env = append(environment, "XDG_RUNTIME_DIR="+directory)
}
