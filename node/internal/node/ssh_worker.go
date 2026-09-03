package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	pty "github.com/aymanbagabas/go-pty"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type sshPipeConn struct {
	io.Reader
	io.Writer
	close func() error
}
type sshPipeAddr string

func (a sshPipeAddr) Network() string       { return "pipe" }
func (a sshPipeAddr) String() string        { return string(a) }
func (c *sshPipeConn) Close() error         { return c.close() }
func (c *sshPipeConn) LocalAddr() net.Addr  { return sshPipeAddr("worker") }
func (c *sshPipeConn) RemoteAddr() net.Addr { return sshPipeAddr("relay") }

// Handshake/cancellation are bounded by explicitly closing pipes. Anonymous
// Windows pipes do not implement deadlines, so no caller relies on these methods.
func (c *sshPipeConn) SetDeadline(time.Time) error      { return nil }
func (c *sshPipeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *sshPipeConn) SetWriteDeadline(time.Time) error { return nil }

func RunSSHWorker(ctx context.Context) error {
	reader := bufio.NewReaderSize(os.Stdin, 64*1024)
	bootstrap, err := reader.ReadSlice('\n')
	if err != nil {
		return fmt.Errorf("invalid SSH worker bootstrap")
	}
	var config sshWorkerConfig
	if err := json.Unmarshal(bootstrap, &config); err != nil {
		return fmt.Errorf("invalid SSH worker bootstrap")
	}
	if len(config.Roots) == 0 || len(config.Roots) > 32 {
		return fmt.Errorf("invalid SSH worker roots")
	}
	conn := &sshPipeConn{Reader: reader, Writer: os.Stdout, close: func() error { _ = os.Stdin.Close(); return os.Stdout.Close() }}
	defer conn.Close()
	return serveSSH(ctx, conn, config)
}

func serveSSH(ctx context.Context, transport net.Conn, config sshWorkerConfig) error {
	host, err := nodeSSHSigner(config.Token, "host")
	if err != nil {
		return err
	}
	clientKey, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(config.ClientPublicKey))
	if err != nil || len(rest) != 0 || clientKey.Type() != ssh.KeyAlgoED25519 {
		return fmt.Errorf("invalid authorized SSH client key")
	}
	serverConfig := &ssh.ServerConfig{MaxAuthTries: 3, ServerVersion: "SSH-2.0-Mira",
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if meta.User() != "mira" || !bytes.Equal(key.Marshal(), clientKey.Marshal()) {
				return nil, fmt.Errorf("SSH identity denied")
			}
			return &ssh.Permissions{}, nil
		}}
	serverConfig.AddHostKey(host)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() { transport.Close() })
	defer stopClose()
	timer := time.AfterFunc(15*time.Second, func() { transport.Close() })
	conn, channels, requests, err := ssh.NewServerConn(transport, serverConfig)
	timer.Stop()
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		for request := range requests {
			_ = request.Reply(request.Type == "keepalive@openssh.com", nil)
		}
	}()
	go func() { _ = conn.Wait(); cancel() }()
	slots := make(chan struct{}, 4)
	var workers sync.WaitGroup
	defer workers.Wait()
	for channel := range channels {
		if channel.ChannelType() != "session" {
			_ = channel.Reject(ssh.UnknownChannelType, "only session channels are enabled")
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = channel.Reject(ssh.ResourceShortage, "session channel limit reached")
			continue
		}
		ch, reqs, err := channel.Accept()
		if err != nil {
			<-slots
			continue
		}
		workers.Add(1)
		go func() { defer workers.Done(); defer func() { <-slots }(); serveSSHSession(ctx, ch, reqs, config.Roots) }()
	}
	cancel()
	return nil
}

type sshPTYRequest struct {
	Term                         string
	Columns, Rows, Width, Height uint32
	Modes                        string
}

func (r sshPTYRequest) valid() bool {
	return len(r.Term) <= 128 && !strings.ContainsAny(r.Term, "\x00\r\n") && r.Columns > 0 && r.Columns <= 1000 && r.Rows > 0 && r.Rows <= 500 && len(r.Modes) <= 2048
}
func terminalModes(encoded string) (ssh.TerminalModes, error) {
	data := []byte(encoded)
	modes := ssh.TerminalModes{}
	for len(data) > 0 {
		op := data[0]
		data = data[1:]
		if op == 0 || op >= 160 {
			return modes, nil
		}
		if len(data) < 4 {
			return nil, fmt.Errorf("invalid terminal modes")
		}
		modes[op] = binary.BigEndian.Uint32(data[:4])
		data = data[4:]
	}
	return nil, fmt.Errorf("terminal modes missing terminator")
}

func sshShell(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		shell := os.Getenv("COMSPEC")
		if shell == "" {
			shell = "cmd.exe"
		}
		if command == "" {
			return shell, []string{"/d"}
		}
		return shell, []string{"/d", "/s", "/c", command}
	}
	shell := os.Getenv("SHELL")
	if runtime.GOOS == "android" {
		shell = "/system/bin/sh"
	} else if shell == "" {
		shell = "/bin/sh"
	}
	if command == "" {
		return shell, nil
	}
	return shell, []string{"-c", command}
}

func serveSSHSession(parent context.Context, channel ssh.Channel, requests <-chan *ssh.Request, roots []string) {
	defer channel.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() { channel.Close() })
	defer stopClose()
	var terminal *sshPTYRequest
	var running *sshRunningProcess
	finished := make(chan int, 1)
	started := false
	completed := false
	defer func() {
		cancel()
		if started && !completed {
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case status := <-finished:
			completed = true
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
			_ = channel.CloseWrite()
			return
		case request, ok := <-requests:
			if !ok {
				return
			}
			accepted := false
			if len(request.Payload) > 32*1024 {
				_ = request.Reply(false, nil)
				continue
			}
			switch request.Type {
			case "pty-req":
				var value sshPTYRequest
				if !started && terminal == nil && ssh.Unmarshal(request.Payload, &value) == nil && value.valid() {
					if _, err := terminalModes(value.Modes); err == nil {
						terminal = &value
						accepted = true
					}
				}
			case "window-change":
				var value struct{ Columns, Rows, Width, Height uint32 }
				if running != nil && running.resize != nil && ssh.Unmarshal(request.Payload, &value) == nil && value.Columns > 0 && value.Columns <= 1000 && value.Rows > 0 && value.Rows <= 500 {
					accepted = running.resize(int(value.Columns), int(value.Rows)) == nil
				}
			case "signal":
				var value struct{ Signal string }
				if running != nil && ssh.Unmarshal(request.Payload, &value) == nil && (value.Signal == "INT" || value.Signal == "TERM" || value.Signal == "KILL") {
					accepted = running.signal(value.Signal) == nil
				}
			case "shell", "exec":
				var value struct{ Command string }
				valid := request.Type == "shell" && len(request.Payload) == 0
				if request.Type == "exec" {
					valid = ssh.Unmarshal(request.Payload, &value) == nil && value.Command != "" && !strings.ContainsRune(value.Command, 0)
				}
				if !started && valid {
					var err error
					running, err = startSSHProcess(ctx, channel, value.Command, terminal, roots[0])
					if err == nil {
						started, accepted = true, true
						go func() { finished <- running.wait() }()
					} else {
						_, _ = fmt.Fprintln(channel.Stderr(), err)
					}
				}
			case "subsystem":
				var value struct{ Name string }
				if !started && terminal == nil && ssh.Unmarshal(request.Payload, &value) == nil && value.Name == "sftp" {
					fs, err := newSSHFileSystem(roots)
					if err == nil {
						started, accepted = true, true
						go func() {
							server := sftp.NewRequestServer(channel, sftp.Handlers{FileGet: fs, FilePut: fs, FileCmd: fs, FileList: fs}, sftp.WithStartDirectory(fs.wirePath(roots[0])))
							_ = server.Serve()
							_ = server.Close()
							finished <- 0
						}()
					}
				}
			}
			_ = request.Reply(accepted, nil)
		}
	}
}

type sshRunningProcess struct {
	wait   func() int
	resize func(int, int) error
	signal func(string) error
}

func sshExitStatus(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := err.(*exec.ExitError); ok && e.ExitCode() >= 0 {
		return e.ExitCode()
	}
	return 255
}

func startSSHProcess(ctx context.Context, channel ssh.Channel, text string, terminal *sshPTYRequest, cwd string) (*sshRunningProcess, error) {
	name, args := sshShell(text)
	if !filepath.IsAbs(name) {
		var err error
		name, err = exec.LookPath(name)
		if err != nil {
			return nil, err
		}
	}
	if terminal == nil {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir, cmd.Stdout, cmd.Stderr = cwd, channel, channel.Stderr()
		cmd.WaitDelay = 2 * time.Second
		configureSSHCommand(cmd)
		input, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			input.Close()
			return nil, err
		}
		go func() { _, _ = io.Copy(input, channel); _ = input.Close() }()
		return &sshRunningProcess{wait: func() int { defer input.Close(); return sshExitStatus(cmd.Wait()) }, signal: func(s string) error { return signalSSHProcess(cmd.Process, s) }}, nil
	}
	tty, err := pty.New()
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	closed := false
	closeTTY := func() {
		mu.Lock()
		defer mu.Unlock()
		if !closed {
			closed = true
			_ = tty.Close()
		}
	}
	if err := tty.Resize(int(terminal.Columns), int(terminal.Rows)); err != nil {
		closeTTY()
		return nil, err
	}
	modes, err := terminalModes(terminal.Modes)
	if err != nil {
		closeTTY()
		return nil, err
	}
	if runtime.GOOS != "windows" {
		if err := pty.ApplyTerminalModes(int(tty.Fd()), int(terminal.Columns), int(terminal.Rows), modes); err != nil {
			closeTTY()
			return nil, err
		}
	}
	cmd := tty.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env = cwd, append(os.Environ(), "TERM="+terminal.Term)
	configureSSHPTY(cmd)
	if err := cmd.Start(); err != nil {
		closeTTY()
		return nil, err
	}
	if unixTTY, ok := tty.(pty.UnixPty); ok {
		_ = unixTTY.Slave().Close()
	}
	reader, closeReader, err := sshPTYReader(tty)
	if err != nil {
		closeTTY()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	stopClose := context.AfterFunc(ctx, closeTTY)
	go func() { _, _ = io.Copy(tty, channel) }()
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(channel, reader); close(drained) }()
	return &sshRunningProcess{
		resize: func(cols, rows int) error {
			mu.Lock()
			defer mu.Unlock()
			if closed {
				return os.ErrClosed
			}
			return tty.Resize(cols, rows)
		},
		signal: func(s string) error { return signalSSHProcess(cmd.Process, s) },
		wait: func() int {
			defer closeReader()
			err := cmd.Wait()
			if runtime.GOOS == "windows" {
				closeTTY()
			}
			select {
			case <-drained:
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
			stopClose()
			closeTTY()
			return sshExitStatus(err)
		},
	}, nil
}
