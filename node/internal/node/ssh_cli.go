package node

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func (client *cliClient) sshTransport(ctx context.Context, selector string) (*sshWebSocketConn, ssh.PublicKey, error) {
	node, err := client.resolveNode(ctx, selector)
	if err != nil {
		return nil, nil, err
	}
	keys, err := sshPublicKeys(client.identity.Token)
	if err != nil {
		return nil, nil, err
	}
	if err := client.request(ctx, http.MethodPost, "/v1/nodes/"+client.identity.NodeID+"/ssh/keys", keys, nil); err != nil {
		return nil, nil, err
	}
	var session struct {
		SessionID       string `json:"sessionId"`
		HostKey         string `json:"hostKey"`
		ProtocolVersion int    `json:"protocolVersion"`
	}
	if err := client.request(ctx, http.MethodPost, "/v1/nodes/"+selectorValue(node, "nodeId")+"/ssh/sessions", nil, &session); err != nil {
		return nil, nil, err
	}
	host, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(session.HostKey))
	if err != nil || len(rest) != 0 || session.ProtocolVersion != 1 || host.Type() != ssh.KeyAlgoED25519 {
		return nil, nil, fmt.Errorf("Server returned invalid SSH session identity")
	}
	conn, err := dialSSHTransport(ctx, client.identity.ServerURL, client.identity.Token, session.SessionID, "source")
	return conn, host, err
}

func (client *cliClient) connectSSH(ctx context.Context, selector string) (*ssh.Client, error) {
	connectCtx, cancel := context.WithTimeout(ctx, client.options.Timeout)
	defer cancel()
	conn, host, err := client.sshTransport(connectCtx, selector)
	if err != nil {
		return nil, err
	}
	signer, err := nodeSSHSigner(client.identity.Token, "client")
	if err != nil {
		conn.Close()
		return nil, err
	}
	stopClose := context.AfterFunc(connectCtx, func() { conn.Close() })
	config := &ssh.ClientConfig{User: "mira", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if !bytes.Equal(host.Marshal(), key.Marshal()) {
				return fmt.Errorf("SSH host key does not match the approved target Node")
			}
			return nil
		}}
	connection, channels, requests, err := ssh.NewClientConn(conn, selector, config)
	stopClose()
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(connection, channels, requests), nil
}

func (client *cliClient) runSSHCommands(ctx context.Context, command string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	// Use platform CAs (notably Android's explicit system bundle) for key discovery too.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: platformCertificatePool()}
	client.http.Transport = transport
	if command == "scp" {
		return client.runSCP(ctx, args, stdout)
	}
	if command == "sftp" {
		return client.runSFTP(ctx, args, stdin, stdout, stderr)
	}
	set := flagSet("ssh")
	forcePTY := set.Bool("t", false, "force PTY")
	noPTY := set.Bool("T", false, "disable PTY")
	if err := set.Parse(args); err != nil {
		return err
	}
	args = set.Args()
	if len(args) == 0 || (*forcePTY && *noPTY) {
		return fmt.Errorf("usage: mira ssh [-t|-T] <node> [-- command]")
	}
	selector := args[0]
	args = args[1:]
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	conn, err := client.connectSSH(ctx, selector)
	if err != nil {
		return err
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopClose()
	stopKeepalive := sshKeepalive(conn)
	defer stopKeepalive()
	session, err := conn.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	input, err := session.StdinPipe()
	if err != nil {
		return err
	}
	session.Stdout, session.Stderr = stdout, stderr
	file, isFile := stdin.(*os.File)
	interactive := isFile && term.IsTerminal(int(file.Fd()))
	usePTY := *forcePTY || (!*noPTY && len(args) == 0 && interactive)
	if usePTY {
		cols, rows := 80, 24
		if interactive {
			if w, h, e := term.GetSize(int(file.Fd())); e == nil {
				cols, rows = w, h
			}
		}
		if err := session.RequestPty("xterm-256color", rows, cols, ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 38400, ssh.TTY_OP_OSPEED: 38400}); err != nil {
			return err
		}
		if interactive {
			state, err := term.MakeRaw(int(file.Fd()))
			if err != nil {
				return err
			}
			defer term.Restore(int(file.Fd()), state)
			resizeCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			go func() {
				ticker := time.NewTicker(250 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-resizeCtx.Done():
						return
					case <-ticker.C:
						w, h, e := term.GetSize(int(file.Fd()))
						if e == nil && (w != cols || h != rows) {
							cols, rows = w, h
							_ = session.WindowChange(rows, cols)
						}
					}
				}
			}()
		}
	}
	if len(args) == 0 {
		err = session.Shell()
	} else {
		err = session.Start(strings.Join(args, " "))
	}
	if err != nil {
		return err
	}
	go func() { _, _ = io.Copy(input, stdin); _ = input.Close() }()
	return session.Wait()
}

func sshKeepalive(client *ssh.Client) func() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				timer := time.AfterFunc(15*time.Second, func() { client.Close() })
				_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
				timer.Stop()
				if err != nil {
					client.Close()
					return
				}
			}
		}
	}()
	return cancel
}

func (client *cliClient) connectSFTP(ctx context.Context, selector string) (*sftp.Client, func(), error) {
	conn, err := client.connectSSH(ctx, selector)
	if err != nil {
		return nil, nil, err
	}
	stopClose := context.AfterFunc(ctx, func() { conn.Close() })
	keepalive := sshKeepalive(conn)
	close := func() { stopClose(); keepalive(); conn.Close() }
	fs, err := sftp.NewClient(conn)
	if err != nil {
		close()
		return nil, nil, err
	}
	return fs, func() { fs.Close(); close() }, nil
}

func splitRemoteOperand(value string) (string, string, bool) {
	// Double-colon permits full nodeKeys containing colons. Single-colon is
	// convenient for hostnames/UUIDs; C:\local and C:/local stay local on Windows.
	if index := strings.Index(value, "::"); index > 0 {
		return value[:index], value[index+2:], true
	}
	index := strings.IndexByte(value, ':')
	if index <= 0 || (index == 1 && len(value) > 2 && (value[2] == '/' || value[2] == '\\')) {
		return "", value, false
	}
	return value[:index], value[index+1:], true
}

func (client *cliClient) runSCP(ctx context.Context, args []string, stdout io.Writer) error {
	set := flagSet("scp")
	overwrite := set.Bool("overwrite", false, "replace destination")
	if err := set.Parse(args); err != nil {
		return err
	}
	args = set.Args()
	if len(args) != 2 {
		return fmt.Errorf("usage: mira scp [--overwrite] <local> <node:/path> (or reverse); regular files only")
	}
	sourceNode, source, sourceRemote := splitRemoteOperand(args[0])
	targetNode, target, targetRemote := splitRemoteOperand(args[1])
	if sourceRemote == targetRemote {
		return fmt.Errorf("scp requires exactly one remote operand; use nodeKey::/path for keys containing colons")
	}
	selector := sourceNode
	if targetRemote {
		selector = targetNode
	}
	fs, close, err := client.connectSFTP(ctx, selector)
	if err != nil {
		return err
	}
	defer close()
	if sourceRemote {
		err = downloadSSHFile(fs, source, target, *overwrite)
	} else {
		err = uploadSSHFile(fs, source, target, *overwrite)
	}
	if err == nil {
		_, _ = fmt.Fprintln(stdout, "Transferred", args[0], "->", args[1])
	}
	return err
}

func uploadSSHFile(fs *sftp.Client, local, remote string, overwrite bool) error {
	source, err := os.Open(local)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source must be a regular file")
	}
	if info, err := fs.Stat(remote); err == nil && info.IsDir() {
		remote = path.Join(remote, filepath.Base(local))
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if overwrite {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	dest, err := fs.OpenFile(remote, flags)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyBuffer(dest, source, make([]byte, 32*1024))
	closeErr := dest.Close()
	return errors.Join(copyErr, closeErr)
}
func downloadSSHFile(fs *sftp.Client, remote, local string, overwrite bool) error {
	source, err := fs.Open(remote)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source must be a regular file")
	}
	if info, err := os.Stat(local); err == nil && info.IsDir() {
		local = filepath.Join(local, path.Base(remote))
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if overwrite {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	dest, err := os.OpenFile(local, flags, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyBuffer(dest, source, make([]byte, 32*1024))
	closeErr := dest.Close()
	return errors.Join(copyErr, closeErr)
}

func (client *cliClient) runSFTP(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: mira sftp <node> [ls|stat|mkdir|rm|get|put ...]")
	}
	fs, close, err := client.connectSFTP(ctx, args[0])
	if err != nil {
		return err
	}
	defer close()
	cwd, err := fs.Getwd()
	if err != nil {
		return err
	}
	run := func(words []string) error {
		if len(words) == 0 {
			return nil
		}
		remote := cwd
		if len(words) > 1 {
			remote = words[1]
			if !path.IsAbs(remote) {
				remote = path.Join(cwd, remote)
			}
		}
		switch words[0] {
		case "pwd":
			fmt.Fprintln(stdout, cwd)
			return nil
		case "help":
			fmt.Fprintln(stdout, "pwd | ls [path] | cd path | stat path | mkdir path | rm path | get remote local | put local remote | exit\nQuote paths containing spaces. Existing files are not overwritten.")
			return nil
		case "ls":
			entries, err := fs.ReadDir(remote)
			if err != nil {
				return err
			}
			for _, info := range entries {
				fmt.Fprintf(stdout, "%s %12d %s\n", info.Mode(), info.Size(), info.Name())
			}
			return nil
		case "cd":
			info, err := fs.Stat(remote)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return fmt.Errorf("not a directory")
			}
			cwd = remote
			return nil
		case "stat":
			info, err := fs.Stat(remote)
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s %d %s\n", info.Mode(), info.Size(), remote)
			return nil
		case "mkdir":
			if len(words) == 2 {
				return fs.Mkdir(remote)
			}
		case "rm":
			if len(words) == 2 {
				return fs.Remove(remote)
			}
		case "get":
			if len(words) == 3 {
				return downloadSSHFile(fs, remote, words[2], false)
			}
		case "put":
			if len(words) == 3 {
				dest := words[2]
				if !path.IsAbs(dest) {
					dest = path.Join(cwd, dest)
				}
				return uploadSSHFile(fs, words[1], dest, false)
			}
		}
		return fmt.Errorf("invalid SFTP command; use help")
	}
	if len(args) > 1 {
		return run(args[1:])
	}
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 1024), 32768)
	for {
		fmt.Fprint(stdout, "mira-sftp> ")
		if !scanner.Scan() {
			return scanner.Err()
		}
		words, err := sshCommandWords(scanner.Text())
		if err == nil && len(words) > 0 && (words[0] == "exit" || words[0] == "quit") {
			return nil
		}
		if err == nil {
			err = run(words)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
		}
	}
}

func sshCommandWords(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	var quote rune
	started := false
	for _, r := range line {
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			started = true
			continue
		}
		if r == ' ' || r == '\t' {
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
			continue
		}
		word.WriteRune(r)
		started = true
	}
	if quote != 0 {
		return nil, fmt.Errorf("unclosed quote")
	}
	if started {
		words = append(words, word.String())
	}
	return words, nil
}
