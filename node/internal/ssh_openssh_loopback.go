package node

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// Win32 OpenSSH's inetd path does not support ordinary pipe handles reliably.
// Use one private loopback listener per approved relay, never a public sshd.
// Even a same-host port race cannot impersonate the target: its host key is pinned.
func runOpenSSHLoopback(ctx context.Context, program, config string, in io.Reader, out io.Writer) error {
	reservation, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	address := reservation.Addr().String()
	port := reservation.Addr().(*net.TCPAddr).Port
	reservation.Close()
	command := backgroundCommand(exec.Command(program, "-D", "-e", "-f", config, "-p", strconv.Itoa(port), "-o", "ListenAddress=127.0.0.1"))
	if os.Getenv("MIRA_OPENSSH_DEBUG") == "1" {
		command.Args = append(command.Args, "-ddd")
	}
	command.Stdout, command.Stderr = os.Stderr, os.Stderr
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
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	defer func() {
		command.Process.Kill()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}()
	var conn net.Conn
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = net.DialTimeout("tcp4", address, 100*time.Millisecond)
		if err == nil {
			break
		}
		select {
		case e := <-done:
			return fmt.Errorf("private OpenSSH listener exited: %v", e)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Millisecond):
		}
	}
	if err != nil {
		return fmt.Errorf("private OpenSSH listener unavailable: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	go func() { io.Copy(conn, in); conn.Close() }()
	_, err = io.Copy(out, conn)
	return err
}
