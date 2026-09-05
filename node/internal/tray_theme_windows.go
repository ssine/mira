package node

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Activate native v6 controls on the tray's UI thread. A thread-local activation
// context works for both the development Go image and the single OpenSSH-linked
// image without changing the console subsystem or the other executable roles.
func activateTrayTheme() func() {
	noop := func() {}
	file, err := os.CreateTemp("", "mira-tray-*.manifest")
	if err != nil {
		return noop
	}
	defer os.Remove(file.Name())
	_, err = file.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
<assemblyIdentity version="1.0.0.0" processorArchitecture="*" name="Mira.Node.Tray" type="win32"/>
<dependency><dependentAssembly><assemblyIdentity type="win32" name="Microsoft.Windows.Common-Controls" version="6.0.0.0" processorArchitecture="*" publicKeyToken="6595b64144ccf1df" language="*"/></dependentAssembly></dependency>
</assembly>`)
	file.Close()
	if err != nil {
		return noop
	}
	context := struct {
		size, flags                                      uint32
		source                                           *uint16
		architecture, language                           uint16
		assemblyDirectory, resourceName, applicationName *uint16
		module                                           uintptr
	}{source: trayUTF16(file.Name())}
	context.size = uint32(unsafe.Sizeof(context))
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	handle, _, _ := kernel.NewProc("CreateActCtxW").Call(uintptr(unsafe.Pointer(&context)))
	if handle == ^uintptr(0) {
		return noop
	}
	var cookie uintptr
	ok, _, _ := kernel.NewProc("ActivateActCtx").Call(handle, uintptr(unsafe.Pointer(&cookie)))
	if ok == 0 {
		kernel.NewProc("ReleaseActCtx").Call(handle)
		return noop
	}
	return func() {
		kernel.NewProc("DeactivateActCtx").Call(0, cookie)
		kernel.NewProc("ReleaseActCtx").Call(handle)
	}
}
