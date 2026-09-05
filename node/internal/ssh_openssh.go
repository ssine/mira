package node

import (
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Set only by the native linking pipeline. A plain Go build is development-only
// and must never silently select a different SSH implementation.
var BundledOpenSSH = "false"

// Resolve role aliases to this exact image; never use PATH or a system sshd.
func openSSHProgram(role string) (string, error) {
	if BundledOpenSSH != "true" {
		return "", fmt.Errorf("this development build has no embedded OpenSSH; install a linked Mira release")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" && runtime.GOOS != "android" {
		return "", fmt.Errorf("native OpenSSH backend has not been validated on %s", runtime.GOOS)
	}
	if runtime.GOOS == "android" && os.Geteuid() == 0 && os.Getenv("MIRA_NODE_OPENSSH_ANDROID_ROOT") != "1" {
		return "", fmt.Errorf("native OpenSSH Android root mode requires a root-capable bundled image")
	}
	dir := os.Getenv("MIRA_NODE_OPENSSH_DIR")
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if dir == "" {
		dir = filepath.Dir(self)
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("MIRA_NODE_OPENSSH_DIR must be an absolute path")
	}
	if runtime.GOOS == "windows" {
		role += ".exe"
	}
	p := filepath.Join(dir, role)
	info, err := os.Stat(p)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("bundled OpenSSH program unavailable: %s", role)
	}
	selfInfo, err := os.Stat(self)
	if err != nil || !os.SameFile(info, selfInfo) {
		return "", fmt.Errorf("OpenSSH role must link to the running Mira image: %s", role)
	}
	return p, nil
}

func openSSHUsername() (string, error) {
	if runtime.GOOS == "android" {
		// Go's os/user intentionally reports a synthetic "android" account.
		// Ask Android's libc-backed utility for the real app/root account.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		b, err := exec.CommandContext(ctx, "/system/bin/id", "-un").Output()
		name := strings.TrimSpace(string(b))
		if err != nil || name == "" || strings.ContainsAny(name, "\x00\r\n\"") {
			return "", fmt.Errorf("resolve Android SSH account: %v", err)
		}
		return name, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current SSH OS account: %w", err)
	}
	name := u.Username
	if runtime.GOOS == "windows" {
		name = strings.ToLower(name)
		// Win32 OpenSSH canonicalizes local accounts without the computer name.
		if computer, e := os.Hostname(); e == nil {
			domain, account, qualified := strings.Cut(name, `\`)
			if qualified && strings.EqualFold(domain, computer) {
				name = account
			}
		}
	}
	if name == "" || len(name) > 256 || strings.ContainsAny(name, "\x00\r\n") {
		return "", fmt.Errorf("invalid SSH OS account")
	}
	return name, nil
}

func openSSHRuntime() map[string]any {
	name, err := openSSHUsername()
	if err != nil {
		return map[string]any{"backend": "openssh", "error": err.Error()}
	}
	if _, err = openSSHProgram("sshd"); err != nil {
		return map[string]any{"backend": "openssh", "error": err.Error()}
	}
	return map[string]any{"backend": "openssh", "username": name}
}

func sshConfigString(s string) (string, error) {
	if strings.ContainsAny(s, "\x00\r\n\"") {
		return "", fmt.Errorf("invalid SSH config value")
	}
	// OpenSSH's config parser preserves backslashes (not JSON escaping).
	// In particular, Windows account names contain a literal domain separator.
	return `"` + s + `"`, nil
}

func privateSSHFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = protectIdentityFile(f); err != nil {
		return err
	}
	_, err = f.Write(data)
	return err
}

func privateSSHDirectory(parent string) (string, error) {
	if !filepath.IsAbs(parent) {
		return "", fmt.Errorf("SSH state directory must be absolute")
	}
	dir, err := os.MkdirTemp(parent, "ssh-session-")
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		f, e := os.Open(dir)
		err = e
		if err == nil {
			err = protectIdentityFile(f)
			f.Close()
		}
	} else {
		err = os.Chmod(dir, 0700)
	}
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func sshPrivateKeyPEM(token, role string) ([]byte, error) {
	key, err := nodeSSHPrivateKey(token, role)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(key, "Mira purpose-separated "+role+" key")
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(block), nil
}

func serveOpenSSH(ctx context.Context, input io.Reader, output io.Writer, config sshWorkerConfig) error {
	// Native SFTP has OS-user scope. Do not silently bypass an existing narrower
	// Mira file policy. SSH is unavailable on deliberately narrowed Nodes.
	if !openSSHFullFilesystem(config.Roots) {
		return fmt.Errorf("embedded OpenSSH is disabled for narrowed allowed roots; use Mira file capabilities instead")
	}
	program, err := openSSHProgram("sshd")
	if err != nil {
		return err
	}
	name, err := openSSHUsername()
	if err != nil {
		return err
	}
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(config.ClientPublicKey))
	if err != nil || len(rest) != 0 || key.Type() != ssh.KeyAlgoED25519 {
		return fmt.Errorf("invalid authorized SSH client key")
	}
	state, err := openSSHStateDirectory(config.StateDirectory)
	if err != nil {
		return err
	}
	dir, err := privateSSHDirectory(state)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	host, err := sshPrivateKeyPEM(config.Token, "host")
	if err != nil {
		return err
	}
	if err = privateSSHFile(filepath.Join(dir, "host"), host); err != nil {
		return err
	}
	clear(host)
	if err = privateSSHFile(filepath.Join(dir, "authorized_keys"), ssh.MarshalAuthorizedKey(key)); err != nil {
		return err
	}
	// Mira supplies the exact approved caller key in an owner-private session
	// directory. Do not apply sshd's home/ancestor ownership heuristic to this
	// generated file: an otherwise valid Node state parent may be group-writable.
	settings := []string{"PasswordAuthentication no", "KbdInteractiveAuthentication no", "AuthenticationMethods publickey", "PubkeyAuthentication yes", "PermitRootLogin prohibit-password", "StrictModes no", "PermitUserEnvironment no", "PermitUserRC no", "X11Forwarding no", "UseDNS no", "LoginGraceTime 15", "LogLevel ERROR"}
	subsystem := "internal-sftp"
	if runtime.GOOS == "windows" {
		p, e := openSSHProgram("sftp-server")
		if e != nil {
			return e
		}
		subsystem, err = sshConfigString(filepath.ToSlash(p))
		if err != nil {
			return err
		}
	}
	settings = append(settings, "Subsystem sftp "+subsystem)
	for _, entry := range [][2]string{{"HostKey", filepath.Join(dir, "host")}, {"AuthorizedKeysFile", filepath.Join(dir, "authorized_keys")}, {"PidFile", filepath.Join(dir, "pid")}, {"AllowUsers", name}} {
		v := entry[1]
		if entry[0] != "AllowUsers" {
			v = filepath.ToSlash(v)
		}
		value, err := sshConfigString(v)
		if err != nil {
			return err
		}
		settings = append(settings, entry[0]+" "+value)
	}
	for _, helper := range [][2]string{{"SshdSessionPath", "sshd-session"}, {"SshdAuthPath", "sshd-auth"}} {
		if p, err := openSSHProgram(helper[1]); err == nil {
			q, _ := sshConfigString(filepath.ToSlash(p))
			settings = append(settings, helper[0]+" "+q)
		}
	}
	path := filepath.Join(dir, "sshd_config")
	if err = privateSSHFile(path, []byte(strings.Join(settings, "\n")+"\n")); err != nil {
		return err
	}
	cleanup, err := prepareOpenSSHWorker()
	if err != nil {
		return err
	}
	defer cleanup()
	if runtime.GOOS == "windows" {
		return runOpenSSHLoopback(ctx, program, path, input, output)
	}
	args := []string{"-i", "-e", "-f", path}
	if os.Getenv("MIRA_OPENSSH_DEBUG") == "1" {
		args = append(args, "-ddd")
	}
	command := exec.Command(program, args...)
	if runtime.GOOS == "android" {
		command.Env = append(os.Environ(), "MIRA_OPENSSH_APP_HOME="+state)
		if os.Geteuid() == 0 {
			empty := filepath.Join(dir, "empty")
			if err := os.Mkdir(empty, 0700); err != nil {
				return err
			}
			command.Env = append(command.Env, "MIRA_NODE_OPENSSH_PRIVSEP_DIR="+empty)
		}
	}
	command.Stdout, command.Stderr = output, os.Stderr
	command.WaitDelay = 500 * time.Millisecond
	pipe, err := command.StdinPipe()
	if err != nil {
		return err
	}
	defer pipe.Close()
	if err = command.Start(); err != nil {
		return err
	}
	guard, err := guardSSHProcessTree(command.Process)
	if err != nil {
		command.Process.Kill()
		command.Wait()
		return err
	}
	defer guard()
	go func() { _, _ = io.Copy(pipe, input); pipe.Close(); _ = command.Process.Kill() }()
	stop := context.AfterFunc(ctx, func() { _ = command.Process.Kill() })
	defer stop()
	return command.Wait()
}

func openSSHFullFilesystem(roots []string) bool {
	wanted := defaultAllowedRoots()
	for _, full := range wanted {
		found := false
		for _, root := range roots {
			if filepath.Clean(root) == filepath.Clean(full) || (runtime.GOOS == "windows" && strings.EqualFold(filepath.Clean(root), filepath.Clean(full))) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return len(wanted) > 0
}
