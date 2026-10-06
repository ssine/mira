package node

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

var singleImageRoles struct {
	sync.Mutex
	self, parent, directory string
	info                    os.FileInfo
}

// A linked image contains every OpenSSH role. When copied without archive
// aliases, create only verified links in an owner-private immutable version
// directory. Never download or select a system SSH executable.
func singleImageOpenSSHRoles(self string) (string, error) {
	identity, err := DefaultIdentityFile()
	if err != nil {
		return "", err
	}
	return ensureSingleImageOpenSSHRoles(self, filepath.Join(filepath.Dir(identity), "runtimes", "openssh"))
}
func ensureSingleImageOpenSSHRoles(self, parent string) (string, error) {
	singleImageRoles.Lock()
	defer singleImageRoles.Unlock()
	resolved, err := filepath.EvalSymlinks(self)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("invalid linked Mira image")
	}
	if singleImageRoles.self == resolved && singleImageRoles.parent == parent && singleImageRoles.info != nil && os.SameFile(info, singleImageRoles.info) && info.Size() == singleImageRoles.info.Size() && info.ModTime() == singleImageRoles.info.ModTime() {
		return singleImageRoles.directory, nil
	}
	input, err := os.Open(resolved)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, err = io.Copy(digest, input)
	input.Close()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(parent, Version, runtime.GOOS+"-"+runtime.GOARCH+"-"+hex.EncodeToString(digest.Sum(nil)))
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("prepare embedded SSH role links: %w", err)
	}
	if runtime.GOOS == "windows" {
		f, openErr := os.Open(dir)
		if openErr != nil {
			return "", openErr
		}
		err = protectIdentityFile(f)
		f.Close()
	} else {
		err = os.Chmod(dir, 0700)
	}
	if err != nil {
		return "", fmt.Errorf("protect embedded SSH role directory: %w", err)
	}
	names := []string{"mira", "ssh", "sshd", "sshd-session", "sshd-auth", "scp", "sftp", "sftp-server", "ssh-keygen"}
	if runtime.GOOS == "windows" {
		names = append(names, "ssh-shellhost", "ssh-agent", "ssh-add", "ssh-keyscan", "ssh-sk-helper", "ssh-pkcs11-helper")
	}
	for _, name := range names {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		path := filepath.Join(dir, name)
		existing, statErr := os.Stat(path)
		if os.IsNotExist(statErr) {
			if runtime.GOOS == "windows" {
				err = os.Link(resolved, path)
			} else {
				err = os.Symlink(resolved, path)
			}
			// A concurrent process can publish the same verified role link first.
			if err != nil && !os.IsExist(err) {
				return "", fmt.Errorf("create embedded SSH role %s: %w", name, err)
			}
			existing, statErr = os.Stat(path)
		}
		if statErr != nil || !os.SameFile(info, existing) {
			return "", fmt.Errorf("embedded SSH role %s does not reference this Mira image", name)
		}
	}
	singleImageRoles.self, singleImageRoles.parent, singleImageRoles.directory, singleImageRoles.info = resolved, parent, dir, info
	return dir, nil
}
