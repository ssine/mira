//go:build windows

package node

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsIdentityACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	configuration := config{ServerURL: "https://mira.example.test", IdentityFile: path}
	if _, err := loadOrCreateNodeState(configuration, nodeIdentity{NodeKey: "windows-test"}); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	sddl := descriptor.String()
	if !strings.Contains(sddl, "D:P") || strings.Contains(sddl, ";;;WD)") || strings.Contains(sddl, ";;;BU)") || strings.Contains(sddl, ";;;AU)") {
		t.Fatalf("identity DACL is not protected: %s", sddl)
	}
}

func TestWindowsNativeCapabilities(t *testing.T) {
	directory := t.TempDir()
	runtimeValue, err := newCapabilityRuntime(config{AllowedRoots: defaultAllowedRoots()})
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeValue.close()
	ctx := context.Background()
	status, err := runtimeValue.machineStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status["platform"] != "windows" || status["ptyBackend"] != "windows-conpty" || status["cpuCount"].(int) < 1 {
		t.Fatalf("unexpected Windows status: %#v", status)
	}
	if status["memory"].(map[string]any)["totalBytes"].(int64) <= 0 {
		t.Fatal("Windows memory status is empty")
	}
	filePath := filepath.Join(directory, "你好.txt")
	if _, err := runtimeValue.file(fileParams{Action: "write", Path: filePath, Content: "Mira Windows 文件\n"}); err != nil {
		t.Fatal(err)
	}
	read, err := runtimeValue.file(fileParams{Action: "read", Path: filePath})
	if err != nil || read.(map[string]any)["content"] != "Mira Windows 文件\n" {
		t.Fatalf("Windows file round trip failed: %#v %v", read, err)
	}
	if _, err := runtimeValue.process(ctx, processParams{Action: "count"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeValue.process(ctx, processParams{Action: "list", System: true}); err != nil {
		t.Fatal(err)
	}
	started, err := runtimeValue.startProcess(processParams{Command: "cmd.exe", Args: []string{"/d", "/c", "echo MIRA_PROCESS_NATIVE"}, CWD: directory})
	if err != nil {
		t.Fatal(err)
	}
	id := started.(map[string]any)["processId"].(string)
	process, err := runtimeValue.managedProcess(id)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		view := process.view(id, 0)
		if !view["running"].(bool) {
			if text := outputTextForTest(view["output"].(map[string]any)); !strings.Contains(text, "MIRA_PROCESS_NATIVE") {
				t.Fatalf("Windows process output missing marker: %q", text)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Windows process did not exit")
}

func TestWindowsSFTPDefaultRootRealPath(t *testing.T) {
	fs, err := newSSHFileSystem(defaultAllowedRoots())
	if err != nil {
		t.Fatal(err)
	}
	defer fs.runtime.close()
	got, err := fs.RealPath(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0] != '/' || got[2] != ':' || got[3] != '/' {
		t.Fatalf("unexpected Windows SFTP root: %q", got)
	}
}

func outputTextForTest(output map[string]any) string {
	var result strings.Builder
	for _, chunk := range output["chunks"].([]outputChunk) {
		result.WriteString(chunk.Text)
	}
	return result.String()
}

func TestWindowsConPTY(t *testing.T) {
	var output outputBuffer
	handle, err := startPTYProcess("cmd.exe", []string{"/d", "/q"}, t.TempDir(), 24, 80, &output)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.terminate()
	if handle.backend != "windows-conpty" || handle.resize == nil {
		t.Fatalf("not a real ConPTY: %s", handle.backend)
	}
	if err := handle.resize(132, 37); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(handle.stdin, "echo MIRA_CONPTY_NATIVE\r\n"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		text := outputTextForTest(output.read(0))
		if strings.Contains(text, "MIRA_CONPTY_NATIVE") {
			found = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		t.Fatalf("ConPTY did not return interactive output: %q", outputTextForTest(output.read(0)))
	}
	if _, err := io.WriteString(handle.stdin, "exit\r\n"); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { exitCode, _, _ := handle.wait(); done <- exitCode }()
	select {
	case exitCode := <-done:
		if exitCode != 0 {
			t.Fatalf("ConPTY command exited with %d", exitCode)
		}
	case <-time.After(10 * time.Second):
		_ = handle.process.Kill()
		t.Fatal("ConPTY did not shut down")
	}
	if handle.process.Pid == os.Getpid() {
		t.Fatal("ConPTY did not create a child process")
	}
}
