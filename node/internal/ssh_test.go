package node

import (
	"bytes"
	"testing"
)

func TestSSHIdentitySeparation(t *testing.T) {
	_, token, err := newNodeCredential()
	if err != nil {
		t.Fatal(err)
	}
	host, _ := nodeSSHSigner(token, "host")
	client, _ := nodeSSHSigner(token, "client")
	again, _ := nodeSSHSigner(token, "host")
	if bytes.Equal(host.PublicKey().Marshal(), client.PublicKey().Marshal()) {
		t.Fatal("role keys overlap")
	}
	if !bytes.Equal(host.PublicKey().Marshal(), again.PublicKey().Marshal()) {
		t.Fatal("host key changed")
	}
	_, other, _ := newNodeCredential()
	wrong, _ := nodeSSHSigner(other, "host")
	if bytes.Equal(host.PublicKey().Marshal(), wrong.PublicKey().Marshal()) {
		t.Fatal("node keys overlap")
	}
}
func TestSSHRemoteOperands(t *testing.T) {
	node, p, remote := splitRemoteOperand("android:phone::/sdcard/file")
	if !remote || node != "android:phone" || p != "/sdcard/file" {
		t.Fatal(node, p, remote)
	}
	if _, _, remote := splitRemoteOperand("C:/Users/local"); remote {
		t.Fatal("Windows local path parsed as remote")
	}
}
