package main

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

const shellShowNormal = 1
const createNoWindow = 0x08000000

var (
	shell32DLL        = syscall.NewLazyDLL("shell32.dll")
	procShellExecuteW = shell32DLL.NewProc("ShellExecuteW")
)

func shellOpen(verb, file, params, dir string) error {
	file = strings.TrimSpace(file)
	if file == "" {
		return fmt.Errorf("path required")
	}
	r, _, err := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(windowsStringOrNil(verb))),
		uintptr(unsafe.Pointer(windowsStringOrNil(file))),
		uintptr(unsafe.Pointer(windowsStringOrNil(params))),
		uintptr(unsafe.Pointer(windowsStringOrNil(dir))),
		uintptr(shellShowNormal),
	)
	if r <= 32 {
		if err != syscall.Errno(0) {
			return err
		}
		return fmt.Errorf("ShellExecuteW failed with code %d", r)
	}
	return nil
}

func windowsStringOrNil(s string) *uint16 {
	if s == "" {
		return nil
	}
	ptr, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return nil
	}
	return ptr
}

func prepareUIServiceCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}
