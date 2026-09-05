package node

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	trayUser32  = windows.NewLazySystemDLL("user32.dll")
	trayShell32 = windows.NewLazySystemDLL("shell32.dll")
	trayGDI32   = windows.NewLazySystemDLL("gdi32.dll")
)

func SupportsTray() bool { return true }

const (
	trayCallback = 0x8001
	trayFinished = 0x8002
	trayShow     = 1001
	trayWeb      = 1002
	trayLogs     = 1003
	trayExit     = 1004
)

type trayPoint struct{ x, y int32 }
type trayRect struct{ left, top, right, bottom int32 }
type trayMessage struct {
	window         uintptr
	message        uint32
	wparam, lparam uintptr
	time           uint32
	point          trayPoint
	private        uint32
}
type trayWindowClass struct {
	size, style                        uint32
	callback                           uintptr
	classExtra, windowExtra            int32
	instance, icon, cursor, background uintptr
	menu, name                         *uint16
	smallIcon                          uintptr
}
type trayNotifyIcon struct {
	size                uint32
	window              uintptr
	id, flags, callback uint32
	icon                uintptr
	tip                 [128]uint16
	state, stateMask    uint32
	info                [256]uint16
	version             uint32
	infoTitle           [64]uint16
	infoFlags           uint32
	guid                windows.GUID
	balloonIcon         uintptr
}

type nodeTray struct {
	window, instance, font uintptr
	className              string
	status                 *desktopStatus
	server, logDirectory   string
	cancel                 context.CancelFunc
	stopping, iconAdded    bool
	icons                  [3]uintptr
	icon                   trayNotifyIcon
	taskbarCreated         uint32
	phase, details         uintptr
	lastPhase, lastDetails string
	controls               []trayControl
	closeTheme             func()
}

type trayControl struct {
	window     uintptr
	x, y, w, h int
}

func trayUTF16(value string) *uint16 { return windows.StringToUTF16Ptr(value) }
func trayCall(name string, args ...uintptr) uintptr {
	result, _, _ := trayUser32.NewProc(name).Call(args...)
	return result
}

// RunTray is an alternate Node entry point, never used by the CLI or OpenSSH
// roles. The installer launches it without a console under the logged-in user.
func RunTray(parent context.Context, args []string) (result error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	show := len(args) > 0 && args[0] == "--show"
	if show {
		args = args[1:]
	}
	configuration, err := loadConfigArgs(args)
	if err != nil {
		trayError(0, "无法读取 Mira 配置，请检查 node.json。")
		return err
	}
	identityPath := strings.ToLower(filepath.Clean(configuration.IdentityFile))
	className := fmt.Sprintf("MiraNodeTray-%x", sha256.Sum256([]byte(identityPath)))
	mutex, err := windows.CreateMutex(nil, false, trayUTF16("Local\\"+className))
	if mutex != 0 {
		defer windows.CloseHandle(mutex)
	}
	if err == windows.ERROR_ALREADY_EXISTS {
		// The first process may still be creating its window. Do not start a
		// second Node, rewrite its identity, or steal its log file.
		for attempt := 0; attempt < 20; attempt++ {
			if window := trayCall("FindWindowW", uintptr(unsafe.Pointer(trayUTF16(className))), 0); window != 0 {
				trayCall("PostMessageW", window, 0x111, trayShow, 0)
				return nil
			}
			time.Sleep(50 * time.Millisecond)
		}
		return nil
	}
	if err != nil {
		return err
	}
	logDirectory := filepath.Join(filepath.Dir(configuration.IdentityFile), "logs")
	closeLog, err := startTrayLog(logDirectory)
	if err != nil {
		trayError(0, "无法打开 Mira 日志目录。")
		return err
	}
	defer closeLog()
	defer func() {
		if result != nil && result != context.Canceled {
			Log("Mira Node tray failed", map[string]any{"error": result.Error()})
		}
	}()
	// Detach only this process, never hide a console shared with the caller.
	windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole").Call()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	ui := &nodeTray{className: className, status: &desktopStatus{phase: "connecting"}, server: desktopServerURL(configuration.ServerURL), logDirectory: logDirectory, cancel: cancel}
	defer ui.destroy()
	if err := ui.create(); err != nil {
		trayError(0, "无法创建 Mira 托盘，请查看日志。")
		return err
	}
	if show {
		ui.show()
	}
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			ui.status.sample()
			if !sleepContext(ctx, time.Second) {
				return
			}
		}
	}()
	done := make(chan error, 1)
	go func() {
		err := runConfigured(ctx, configuration, ui.status)
		if err != nil && err != context.Canceled {
			Log("Mira Node failed", map[string]any{"error": err.Error()})
		}
		done <- err
		trayCall("PostMessageW", ui.window, trayFinished, 0, 0)
	}()
	var message trayMessage
	for {
		value := int32(trayCall("GetMessageW", uintptr(unsafe.Pointer(&message)), 0, 0, 0))
		if value <= 0 {
			if value < 0 {
				result = fmt.Errorf("Windows message loop failed")
			}
			break
		}
		if trayCall("IsDialogMessageW", ui.window, uintptr(unsafe.Pointer(&message))) == 0 {
			trayCall("TranslateMessage", uintptr(unsafe.Pointer(&message)))
			trayCall("DispatchMessageW", uintptr(unsafe.Pointer(&message)))
		}
	}
	cancel()
	// Wait for the existing Node shutdown path (App Server, PTYs and SSH
	// workers) instead of exiting while its cleanup is still running.
	err = <-done
	<-sampled
	Log("Mira Node stopped", nil)
	if result != nil {
		return result
	}
	if err != nil && err != context.Canceled {
		trayError(ui.window, "Mira Node 已停止。请查看日志了解原因。")
		return err
	}
	return nil
}

func startTrayLog(directory string) (func(), error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	log, err := openRotatingLog(filepath.Join(directory, "node.log"), 4*1024*1024, 3)
	if err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		log.Close()
		return nil, err
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = writer, writer
	windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(writer.Fd()))
	windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(writer.Fd()))
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer log.Close()
		if _, err := io.CopyBuffer(log, reader, make([]byte, 32*1024)); err != nil {
			// Keep draining if disk writes fail; otherwise a full pipe could
			// stop Node heartbeats. The user receives a visible error once.
			if !errors.Is(err, os.ErrClosed) {
				go trayError(0, "Mira 日志写入失败，请检查可用磁盘空间和目录权限。")
			}
			io.Copy(io.Discard, reader)
		}
	}()
	return func() {
		os.Stdout, os.Stderr = stdout, stderr
		windows.SetStdHandle(windows.STD_OUTPUT_HANDLE, windows.Handle(stdout.Fd()))
		windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(stderr.Fd()))
		writer.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		reader.Close()
		<-done
	}, nil
}

func trayError(window uintptr, message string) {
	trayCall("MessageBoxW", window, uintptr(unsafe.Pointer(trayUTF16(message))), uintptr(unsafe.Pointer(trayUTF16("Mira Node"))), 0x10)
}

func (ui *nodeTray) create() error {
	// Windows 10 1809+ is our baseline. Native controls retain system colors,
	// high-contrast support and keyboard navigation; sizes follow system DPI.
	trayCall("SetThreadDpiAwarenessContext", ^uintptr(3)) // PER_MONITOR_AWARE_V2
	ui.closeTheme = activateTrayTheme()
	var instance windows.Handle
	err := windows.GetModuleHandleEx(2, nil, &instance)
	if err != nil {
		return err
	}
	ui.instance = uintptr(instance)
	for index := range ui.icons {
		ui.icons[index] = ui.createIcon(index)
	}
	cursor := trayCall("LoadCursorW", 0, 32512)
	class := trayWindowClass{callback: syscall.NewCallback(ui.windowProc), instance: ui.instance, icon: ui.icons[0], smallIcon: ui.icons[0], cursor: cursor, background: 16, name: trayUTF16(ui.className)}
	class.size = uint32(unsafe.Sizeof(class))
	if trayCall("RegisterClassExW", uintptr(unsafe.Pointer(&class))) == 0 {
		return fmt.Errorf("register tray window class")
	}
	ui.window = trayCall("CreateWindowExW", 0x80, uintptr(unsafe.Pointer(class.name)), uintptr(unsafe.Pointer(trayUTF16("Mira Node 状态"))), 0x00C80000, 0x80000000, 0x80000000, 480, 400, 0, 0, ui.instance, 0)
	if ui.window == 0 {
		return fmt.Errorf("create tray window")
	}
	dpi := trayCall("GetDpiForWindow", ui.window)
	if dpi == 0 {
		dpi = 96
	}
	scale := func(value int) uintptr { return uintptr(value) * dpi / 96 }
	rect := trayRect{right: int32(scale(460)), bottom: int32(scale(350))}
	trayCall("AdjustWindowRectExForDpi", uintptr(unsafe.Pointer(&rect)), 0x00C80000, 0, 0x80, dpi)
	trayCall("SetWindowPos", ui.window, 0, 0, 0, uintptr(rect.right-rect.left), uintptr(rect.bottom-rect.top), 0x6)
	height := -int32(scale(15))
	ui.font, _, _ = trayGDI32.NewProc("CreateFontW").Call(uintptr(height), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(trayUTF16("Segoe UI"))))
	control := func(class, text string, style uintptr, x, y, w, h int, id uintptr) uintptr {
		window := trayCall("CreateWindowExW", 0, uintptr(unsafe.Pointer(trayUTF16(class))), uintptr(unsafe.Pointer(trayUTF16(text))), 0x50000000|style, scale(x), scale(y), scale(w), scale(h), ui.window, id, ui.instance, 0)
		trayCall("SendMessageW", window, 0x30, ui.font, 1)
		ui.controls = append(ui.controls, trayControl{window, x, y, w, h})
		return window
	}
	ui.phase = control("STATIC", "正在连接…", 0, 22, 20, 416, 28, 2001)
	control("STATIC", "服务器", 0, 22, 61, 80, 20, 0)
	control("EDIT", ui.server, 0x0800|0x0080|0x10000, 22, 86, 416, 24, 2002) // read-only, selectable
	ui.details = control("STATIC", "", 0, 22, 128, 416, 115, 2003)
	control("STATIC", "关闭此窗口后，Mira 仍会在托盘中运行。", 0, 22, 255, 416, 24, 0)
	control("BUTTON", "打开管理页面", 0x10000, 22, 303, 134, 28, trayWeb)
	control("BUTTON", "查看日志", 0x10000, 168, 303, 116, 28, trayLogs)
	control("BUTTON", "收回托盘", 0x10000, 296, 303, 142, 28, 2)
	ui.taskbarCreated = uint32(trayCall("RegisterWindowMessageW", uintptr(unsafe.Pointer(trayUTF16("TaskbarCreated")))))
	ui.icon = trayNotifyIcon{window: ui.window, id: 1, callback: trayCallback, icon: ui.icons[0]}
	ui.icon.size = uint32(unsafe.Sizeof(ui.icon))
	ui.refresh()
	trayCall("SetTimer", ui.window, 1, 1000, 0)
	return nil
}

func (ui *nodeTray) notify(operation uintptr) bool {
	result, _, _ := trayShell32.NewProc("Shell_NotifyIconW").Call(operation, uintptr(unsafe.Pointer(&ui.icon)))
	return result != 0
}

func (ui *nodeTray) refresh() {
	// Sampling potentially busy process/App Server locks runs off the UI thread.
	view := ui.status.snapshot()
	label := map[string]string{"connecting": "正在连接服务器", "reconnecting": "连接中断 · 正在重试", "online": "已连接", "pending": "等待管理员批准", "rejected": "接入申请已拒绝", "expired": "接入申请已过期 · 正在重试"}[view.Phase]
	if label == "" {
		label = "正在启动"
	}
	if ui.stopping {
		label = "正在退出…"
	}
	codex := map[string]string{"starting": "正在准备", "running": "运行中", "stopped": "未运行", "unsupported": "此设备不支持", "error": "启动失败，请查看日志"}[view.Codex]
	if codex == "" {
		codex = "未运行"
	}
	details := fmt.Sprintf("Codex：%s\r\n活动会话：%d 个进程 · %d 个终端 · %d 个 SSH\r\n版本：%s", codex, view.Processes, view.Terminals, view.SSH, Version)
	if view.Phase == "pending" {
		details += "\r\n接入验证码：" + view.VerificationCode
	}
	if label != ui.lastPhase {
		trayCall("SetWindowTextW", ui.phase, uintptr(unsafe.Pointer(trayUTF16(label))))
		ui.lastPhase = label
	}
	if details != ui.lastDetails {
		trayCall("SetWindowTextW", ui.details, uintptr(unsafe.Pointer(trayUTF16(details))))
		ui.lastDetails = details
	}
	index := 0
	if view.Phase == "online" {
		index = 1
	}
	if view.Phase == "rejected" || view.Codex == "error" {
		index = 2
	}
	ui.icon.icon = ui.icons[index]
	ui.icon.tip = [128]uint16{}
	copy(ui.icon.tip[:], windows.StringToUTF16("Mira Node · "+label))
	ui.icon.flags = 1 | 2 | 4 | 0x80 // MESSAGE | ICON | TIP | SHOWTIP
	if !ui.iconAdded {
		ui.iconAdded = ui.notify(0)
		if ui.iconAdded {
			ui.icon.version = 4
			ui.notify(4)
		}
	} else if !ui.notify(1) {
		ui.iconAdded = false
	}
}

func (ui *nodeTray) show() {
	ui.refresh()
	trayCall("ShowWindow", ui.window, 5)
	trayCall("SetForegroundWindow", ui.window)
}

func (ui *nodeTray) open(target string) {
	if target == "" {
		return
	}
	result, _, _ := trayShell32.NewProc("ShellExecuteW").Call(ui.window, uintptr(unsafe.Pointer(trayUTF16("open"))), uintptr(unsafe.Pointer(trayUTF16(target))), 0, 0, 1)
	if result <= 32 {
		trayError(ui.window, "无法打开，请检查默认浏览器或文件管理器设置。")
	}
}

func (ui *nodeTray) command(id uintptr) {
	switch id {
	case trayShow:
		ui.show()
	case trayWeb:
		ui.open(ui.server)
	case trayLogs:
		ui.open(ui.logDirectory)
	case 2:
		trayCall("ShowWindow", ui.window, 0)
	case trayExit:
		if ui.stopping {
			return
		}
		message := "退出后，此设备将离线，正在本机运行的任务和连接会中断。\n下次登录 Windows 时自动启动。"
		if trayCall("MessageBoxW", ui.window, uintptr(unsafe.Pointer(trayUTF16(message))), uintptr(unsafe.Pointer(trayUTF16("退出 Mira Node？"))), 0x124) != 6 {
			return
		}
		ui.stopping = true
		ui.refresh()
		ui.cancel()
	}
}

func (ui *nodeTray) menu() {
	menu := trayCall("CreatePopupMenu")
	defer trayCall("DestroyMenu", menu)
	for _, item := range []struct {
		id   uintptr
		text string
	}{{trayShow, "查看状态"}, {trayWeb, "打开管理页面"}, {trayLogs, "查看日志"}, {0, ""}, {trayExit, "退出 Mira Node…"}} {
		flags := uintptr(0)
		if item.id == 0 {
			flags = 0x800
		}
		if ui.stopping && item.id == trayExit {
			flags = 1
		}
		trayCall("AppendMenuW", menu, flags, item.id, uintptr(unsafe.Pointer(trayUTF16(item.text))))
	}
	var point trayPoint
	trayCall("GetCursorPos", uintptr(unsafe.Pointer(&point)))
	trayCall("SetForegroundWindow", ui.window)
	id := trayCall("TrackPopupMenu", menu, 0x102, uintptr(point.x), uintptr(point.y), 0, ui.window, 0)
	trayCall("PostMessageW", ui.window, 0, 0, 0)
	ui.command(id)
}

func (ui *nodeTray) windowProc(window uintptr, message uint32, wparam, lparam uintptr) uintptr {
	if ui.taskbarCreated != 0 && message == ui.taskbarCreated {
		// Explorer normally discarded the old icon. Deleting first also makes
		// repeated broadcasts safe instead of retrying ADD on an existing ID.
		ui.notify(2)
		ui.iconAdded = false
		ui.refresh()
		return 0
	}
	switch message {
	case trayCallback:
		switch uint16(lparam) {
		case 0x400, 0x401:
			ui.show()
		case 0x7b:
			ui.menu()
		}
		return 0
	case 0x111:
		ui.command(wparam & 0xffff)
		return 0 // WM_COMMAND
	case 0x10:
		trayCall("ShowWindow", window, 0)
		return 0 // WM_CLOSE only hides
	case 0x113:
		ui.refresh()
		return 0
	case 0x11:
		return 1 // WM_QUERYENDSESSION: don't block logout
	case 0x16:
		if wparam != 0 {
			ui.stopping = true
			ui.cancel()
		}
		return 0
	case 0x2e0: // WM_DPICHANGED
		rect := (*trayRect)(unsafe.Pointer(lparam))
		trayCall("SetWindowPos", window, 0, uintptr(rect.left), uintptr(rect.top), uintptr(rect.right-rect.left), uintptr(rect.bottom-rect.top), 0x14)
		ui.scaleControls(wparam & 0xffff)
		return 0
	case trayFinished:
		trayCall("PostQuitMessage", 0)
		return 0
	}
	return trayCall("DefWindowProcW", window, uintptr(message), wparam, lparam)
}

func (ui *nodeTray) scaleControls(dpi uintptr) {
	if dpi == 0 {
		return
	}
	height := -int32(15 * dpi / 96)
	font, _, _ := trayGDI32.NewProc("CreateFontW").Call(uintptr(height), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(trayUTF16("Segoe UI"))))
	for _, control := range ui.controls {
		trayCall("SetWindowPos", control.window, 0, uintptr(control.x)*dpi/96, uintptr(control.y)*dpi/96, uintptr(control.w)*dpi/96, uintptr(control.h)*dpi/96, 0x14)
		trayCall("SendMessageW", control.window, 0x30, font, 1)
	}
	if ui.font != 0 {
		trayGDI32.NewProc("DeleteObject").Call(ui.font)
	}
	ui.font = font
}

func (ui *nodeTray) destroy() {
	if ui.iconAdded {
		ui.notify(2)
	}
	trayCall("KillTimer", ui.window, 1)
	trayCall("DestroyWindow", ui.window)
	trayCall("UnregisterClassW", uintptr(unsafe.Pointer(trayUTF16(ui.className))), ui.instance)
	if ui.font != 0 {
		trayGDI32.NewProc("DeleteObject").Call(ui.font)
	}
	for _, icon := range ui.icons {
		if icon != 0 {
			trayCall("DestroyIcon", icon)
		}
	}
	if ui.closeTheme != nil {
		ui.closeTheme()
	}
}

// Rasterize the existing server/public/icons/mira.svg geometry, with a small
// status dot. No extra executable, icon library or UI framework is required.
func (ui *nodeTray) createIcon(state int) uintptr {
	pixels := make([]byte, 32*32*4)
	mask := make([]byte, 32*4)
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			r, g, b := byte(0x24), byte(0x57), byte(0xd6)
			sx, sy := x*48/32, y*48/32
			if sx >= 14 && sx < 34 && ((sy >= 14 && sy < 18) || (sy >= 22 && sy < 26) || (sy >= 30 && sy < 34)) {
				r, g, b = 255, 255, 255
			}
			if (sx >= 10 && sx < 17 && sy >= 12 && sy < 21) || (sx >= 31 && sx < 38 && sy >= 27 && sy < 36) {
				r, g, b = 0x9f, 0xc1, 0xff
			}
			if (x-26)*(x-26)+(y-26)*(y-26) < 30 {
				r, g, b = 255, 255, 255
				if (x-26)*(x-26)+(y-26)*(y-26) < 16 {
					r, g, b = 0xdd, 0x98, 0x16
					if state == 1 {
						r, g, b = 0x16, 0x9b, 0x62
					}
					if state == 2 {
						r, g, b = 0xd1, 0x38, 0x38
					}
				}
			}
			index := (y*32 + x) * 4
			pixels[index], pixels[index+1], pixels[index+2], pixels[index+3] = b, g, r, 255
		}
	}
	return trayCall("CreateIcon", ui.instance, 32, 32, 1, 32, uintptr(unsafe.Pointer(&mask[0])), uintptr(unsafe.Pointer(&pixels[0])))
}
