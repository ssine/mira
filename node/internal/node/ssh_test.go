package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func sshTestServer(t *testing.T, roots []string) (string, string, func()) {
	t.Helper()
	_, token, err := newNodeCredential()
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := sshPublicKeys(token)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = serveSSH(ctx, conn, sshWorkerConfig{Token: token, ClientPublicKey: keys["clientKey"], Roots: roots})
			}()
		}
	}()
	return listener.Addr().String(), token, func() { cancel(); listener.Close() }
}
func sshTestClient(t *testing.T, addr, token string) *ssh.Client {
	t.Helper()
	signer, _ := nodeSSHSigner(token, "client")
	host, _ := nodeSSHSigner(token, "host")
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{User: "mira", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey()), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}
func TestSSHIdentitySeparationAndAuthentication(t *testing.T) {
	addr, token, close := sshTestServer(t, []string{t.TempDir()})
	defer close()
	host, _ := nodeSSHSigner(token, "host")
	clientKey, _ := nodeSSHSigner(token, "client")
	if bytes.Equal(host.PublicKey().Marshal(), clientKey.PublicKey().Marshal()) {
		t.Fatal("role keys overlap")
	}
	again, _ := nodeSSHSigner(token, "host")
	if !bytes.Equal(host.PublicKey().Marshal(), again.PublicKey().Marshal()) {
		t.Fatal("host key changed")
	}
	_, other, _ := newNodeCredential()
	wrong, _ := nodeSSHSigner(other, "client")
	for _, cfg := range []*ssh.ClientConfig{
		{User: "mira", Auth: []ssh.AuthMethod{ssh.PublicKeys(wrong)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey())},
		{User: "root", Auth: []ssh.AuthMethod{ssh.PublicKeys(clientKey)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey())},
		{User: "mira", Auth: []ssh.AuthMethod{ssh.PublicKeys(clientKey)}, HostKeyCallback: ssh.FixedHostKey(wrong.PublicKey())},
		{User: "mira", HostKeyCallback: ssh.FixedHostKey(host.PublicKey())},
	} {
		cfg.Timeout = 5 * time.Second
		if conn, err := ssh.Dial("tcp", addr, cfg); err == nil {
			conn.Close()
			t.Fatal("invalid identity was accepted")
		}
	}
	_ = sshTestClient(t, addr, token)
}

func TestSSHExecStreamsExitAndEOF(t *testing.T) {
	addr, token, close := sshTestServer(t, []string{t.TempDir()})
	defer close()
	client := sshTestClient(t, addr, token)
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	var stdout, stderr bytes.Buffer
	session.Stdout, session.Stderr = &stdout, &stderr
	command := "printf OUT; printf ERR >&2; exit 7"
	if runtime.GOOS == "windows" {
		command = "echo OUT & echo ERR 1>&2 & exit /b 7"
	}
	err = session.Run(command)
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 7 || !strings.Contains(stdout.String(), "OUT") || !strings.Contains(stderr.String(), "ERR") {
		t.Fatalf("streams/status: %q %q %v", stdout.String(), stderr.String(), err)
	}
	if runtime.GOOS != "windows" {
		session, _ := client.NewSession()
		defer session.Close()
		session.Stdin = strings.NewReader("binary\x00\xff\n")
		data, err := session.Output("cat")
		if err != nil || string(data) != "binary\x00\xff\n" {
			t.Fatalf("stdin EOF/binary: %x %v", data, err)
		}
	}
}

func TestSSHSFTPBinaryAndPolicy(t *testing.T) {
	root := t.TempDir()
	addr, token, close := sshTestServer(t, []string{root})
	defer close()
	client := sshTestClient(t, addr, token)
	fs, err := sftp.NewClient(client)
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	cwd, err := fs.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := fs.Stat(cwd); err != nil {
			t.Fatalf("stat leaked handles: %v", err)
		}
	}
	remote := cwd + "/roundtrip.bin"
	payload := make([]byte, 5*1024*1024+17)
	_, _ = rand.Read(payload)
	file, err := fs.Create(remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	file, err = fs.Open(remote)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(file)
	file.Close()
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("SFTP binary data mismatch", err)
	}
	if err := fs.Remove(remote); err != nil {
		t.Fatal(err)
	}
	if err := fs.RemoveDirectory(cwd); err == nil {
		t.Fatal("removed configured root")
	}
	if _, err := fs.Open(cwd + "/../outside"); err == nil {
		t.Fatal("escaped configured root")
	}
	if runtime.GOOS != "windows" {
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		if _, err := fs.Open(cwd + "/link/secret"); err == nil {
			t.Fatal("escaped using symlink")
		}
	}
}

func TestSSHNativePTY(t *testing.T) {
	addr, token, close := sshTestServer(t, []string{t.TempDir()})
	defer close()
	client := sshTestClient(t, addr, token)
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.RequestPty("xterm-256color", 27, 91, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
		t.Fatal(err)
	}
	command := "stty size; printf PTY_OK"
	if runtime.GOOS == "windows" {
		command = "echo PTY_OK"
	}
	data, err := session.Output(command)
	if err != nil || !strings.Contains(string(data), "PTY_OK") {
		t.Fatalf("PTY output=%q err=%v", data, err)
	}
	if runtime.GOOS != "windows" && !strings.Contains(string(data), "27 91") {
		t.Fatalf("PTY size incorrect: %q", data)
	}
}

func TestSSHCommandWords(t *testing.T) {
	words, err := sshCommandWords(`put "a b" '/remote c'`)
	if err != nil || len(words) != 3 || words[1] != "a b" || words[2] != "/remote c" {
		t.Fatal(words, err)
	}
	if _, err := sshCommandWords(`get "unfinished`); err == nil {
		t.Fatal("accepted unclosed quote")
	}
	node, p, remote := splitRemoteOperand("android:phone::/sdcard/file")
	if !remote || node != "android:phone" || p != "/sdcard/file" {
		t.Fatal(node, p, remote)
	}
	if _, _, remote := splitRemoteOperand(`C:\Users\local`); remote {
		t.Fatal("Windows local path parsed as remote")
	}
}

func TestSSHOpenSSHInterop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("OpenSSH key ACL setup is tested through the Mira Windows client instead")
	}
	binary, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("optional OpenSSH client not installed")
	}
	root := t.TempDir()
	addr, token, close := sshTestServer(t, []string{root})
	defer close()
	match := nodeTokenPattern.FindStringSubmatch(token)
	secret, _ := base64.RawURLEncoding.DecodeString(match[2])
	seed, _ := hkdf.Key(sha256.New, secret, []byte(match[1]), "mira/ssh/v1/client", 32)
	block, err := ssh.MarshalPrivateKey(ed25519.NewKeyFromSeed(seed), "test only")
	if err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(root, "test-key")
	if err := os.WriteFile(identity, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	host, _ := nodeSSHSigner(token, "host")
	hostname, port, _ := net.SplitHostPort(addr)
	known := filepath.Join(root, "known_hosts")
	if err := os.WriteFile(known, append([]byte("["+hostname+"]:"+port+" "), ssh.MarshalAuthorizedKey(host.PublicKey())...), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+known, "-i", identity, "-p", port, "mira@"+hostname, "printf OPENSSH_OK")
	data, err := cmd.CombinedOutput()
	if err != nil || string(data) != "OPENSSH_OK" {
		t.Fatalf("OpenSSH interop: %s %v", data, err)
	}
}

func TestSSHPTYResizeAndChannelClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix stty probe")
	}
	addr, token, close := sshTestServer(t, []string{t.TempDir()})
	defer close()
	client := sshTestClient(t, addr, token)
	session, _ := client.NewSession()
	defer session.Close()
	var output bytes.Buffer
	session.Stdout = &output
	if err := session.RequestPty("xterm", 24, 80, ssh.TerminalModes{ssh.ECHO: 0}); err != nil {
		t.Fatal(err)
	}
	if err := session.Start("sleep 0.2; stty size"); err != nil {
		t.Fatal(err)
	}
	if err := session.WindowChange(33, 111); err != nil {
		t.Fatal(err)
	}
	if err := session.Wait(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "33 111") {
		t.Fatal(output.String())
	}
}
