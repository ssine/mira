package node

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestOpenSSHRequiresLinkedImage(t *testing.T) {
	previous := BundledOpenSSH
	t.Cleanup(func() { BundledOpenSSH = previous })
	BundledOpenSSH = "false"
	if _, err := openSSHProgram("ssh"); err == nil {
		t.Fatal("ordinary Go build accepted as embedded SSH")
	}
	BundledOpenSSH = "true"
	dir := t.TempDir()
	t.Setenv("MIRA_NODE_OPENSSH_DIR", dir)
	for _, name := range []string{"ssh", "ssh.exe"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not the running image"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := openSSHProgram("ssh"); err == nil {
		t.Fatal("external SSH executable accepted")
	}
}

func TestOpenSSHUsesExistingNodeIdentity(t *testing.T) {
	_, token, err := newNodeCredential()
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"client", "host"} {
		raw, err := sshPrivateKeyPEM(token, role)
		if err != nil {
			t.Fatal(err)
		}
		signer, err := ssh.ParsePrivateKey(raw)
		if err != nil {
			t.Fatal(err)
		}
		original, err := nodeSSHSigner(token, role)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(signer.PublicKey().Marshal(), original.PublicKey().Marshal()) {
			t.Fatal("identity changed")
		}
	}
	a, _ := sshPrivateKeyPEM(token, "client")
	b, _ := sshPrivateKeyPEM(token, "host")
	if bytes.Equal(a, b) {
		t.Fatal("host and client keys must be purpose-separated")
	}
}

func TestOpenSSHManagedOptionsCannotBeOverridden(t *testing.T) {
	cases := [][]string{{"-F", "evil"}, {"-i", "key"}, {"-voProxyCommand=evil"}, {"-vF", "evil"}, {"-vvJjump"}, {"-o", "User=root"}, {"-o", "HostKeyAlias=other"}, {"-o", "Include=other"}, {"-o", "\nProxyCommand=evil"}, {"-o", ""}, {"--evil"}}
	for _, cmd := range []string{"ssh", "scp", "sftp"} {
		for _, args := range cases {
			if _, _, err := splitOpenSSHArgs(cmd, append(args, "target")); err == nil {
				t.Errorf("%s accepted %q", cmd, args)
			}
		}
	}
	for _, cmd := range []string{"scp", "sftp"} {
		if _, _, err := splitOpenSSHArgs(cmd, []string{"-vD", "evil", "target"}); err == nil {
			t.Errorf("%s allowed direct SFTP executable", cmd)
		}
	}
}

func TestOpenSSHNativeOptionsAndCommandBoundary(t *testing.T) {
	opts, args, err := splitOpenSSHArgs("ssh", []string{"-vvtt", "-L", "18080:localhost:80", "-Scontrol", "-oControlPersist=10", "node", "--", "echo", "-F"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(opts, []string{"-v", "-v", "-t", "-t", "-L", "18080:localhost:80", "-S", "control", "-o", "ControlPersist=10"}) {
		t.Fatal(opts)
	}
	if !reflect.DeepEqual(args, []string{"node", "--", "echo", "-F"}) {
		t.Fatal(args)
	}
	opts, _, err = splitOpenSSHArgs("scp", []string{"-rp", "folder", "node:/folder"})
	if err != nil || !reflect.DeepEqual(opts, []string{"-r", "-p"}) {
		t.Fatal(opts, err)
	}
}

func TestOpenSSHConfigAndFilesystemPolicy(t *testing.T) {
	value, err := sshConfigString(`domain\user`)
	if err != nil || value != `"domain\user"` {
		t.Fatal(value, err)
	}
	for _, s := range []string{"x\ny", "x\ry", "x\x00y", "x\"y"} {
		if _, err := sshConfigString(s); err == nil {
			t.Errorf("accepted unsafe value %q", s)
		}
	}
	if !openSSHFullFilesystem(defaultAllowedRoots()) {
		t.Fatal("full OS scope rejected")
	}
	if openSSHFullFilesystem([]string{filepath.Join(t.TempDir(), "narrow")}) {
		t.Fatal("narrow roots silently widened")
	}
}
