package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsTrayWindowLifecycle(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	canceled := false
	ui := &nodeTray{className: fmt.Sprintf("MiraTrayTest-%d", os.Getpid()), status: &desktopStatus{phase: "pending", verificationCode: "123456"}, server: "http://127.0.0.1:9", logDirectory: t.TempDir(), cancel: func() { canceled = true }}
	if err := ui.create(); err != nil {
		t.Fatal(err)
	}
	defer ui.destroy()
	shellPresent := trayCall("FindWindowW", uintptr(unsafe.Pointer(trayUTF16("Shell_TrayWnd"))), 0) != 0
	if shellPresent && !ui.iconAdded {
		t.Fatal("Explorer did not accept the notification icon")
	}
	if trayCall("IsWindowVisible", ui.window) != 0 {
		t.Fatal("status window shown on background startup")
	}
	ui.command(trayShow)
	if trayCall("IsWindowVisible", ui.window) == 0 {
		t.Fatal("status action did not show window")
	}
	buffer := make([]uint16, 512)
	trayCall("GetWindowTextW", ui.details, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	if !strings.Contains(windows.UTF16ToString(buffer), "123456") {
		t.Fatal("approval verification code missing")
	}
	trayCall("SendMessageW", ui.window, 0x10, 0, 0)
	if canceled || trayCall("IsWindowVisible", ui.window) != 0 || trayCall("IsWindow", ui.window) == 0 {
		t.Fatal("closing status stopped/destroyed the Node window")
	}
	ui.status.update("online", "")
	ui.refresh()
	if ui.lastPhase != "已连接" || strings.Contains(ui.lastDetails, "123456") {
		t.Fatal("status did not follow approval/connection transition")
	}
	// Simulate Explorer's broadcast without restarting the user's shell.
	trayCall("SendMessageW", ui.window, uintptr(ui.taskbarCreated), 0, 0)
	if (shellPresent && !ui.iconAdded) || (ui.iconAdded && ui.icon.version != 4) {
		t.Fatal("tray notification protocol was not restored")
	}
	ui.command(trayShow)
	escape := trayMessage{window: ui.window, message: 0x100, wparam: 27}
	trayCall("IsDialogMessageW", ui.window, uintptr(unsafe.Pointer(&escape)))
	if canceled || trayCall("IsWindowVisible", ui.window) != 0 {
		t.Fatal("Escape/close action stopped the Node")
	}
	trayCall("SendMessageW", ui.window, 0x16, 1, 0)
	if !canceled {
		t.Fatal("Windows logout did not initiate Node cleanup")
	}
}

func TestWindowsBackgroundConsole(t *testing.T) {
	if os.Getenv("MIRA_TEST_CONSOLE_PROBE") == "1" {
		window, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
		if window != 0 {
			t.Fatal("background child acquired a console")
		}
		return
	}
	command := backgroundCommand(exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestWindowsBackgroundConsole$"))
	command.Env = append(os.Environ(), "MIRA_TEST_CONSOLE_PROBE=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("background child: %v %s", err, output)
	}
}
