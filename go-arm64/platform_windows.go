//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

var (
	kernel32      = syscall.NewLazyDLL("kernel32.dll")
	procDIOC      = kernel32.NewProc("DeviceIoControl")
	shell32       = syscall.NewLazyDLL("shell32.dll")
	procShellExec = shell32.NewProc("ShellExecuteW")
	procIsAdmin   = shell32.NewProc("IsUserAnAdmin")
)

const isWindows = true

func deviceSize(f *os.File) (int64, error) {
	if st, err := f.Stat(); err == nil && st.Mode().IsRegular() {
		return st.Size(), nil
	}
	var out int64
	var ret uint32
	r, _, e := procDIOC.Call(f.Fd(), 0x7405C, 0, 0, uintptr(unsafe.Pointer(&out)), 8, uintptr(unsafe.Pointer(&ret)), 0)
	if r == 0 {
		return 0, e
	}
	return out, nil
}

func hideCmd(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}

func isAdmin() bool {
	r, _, _ := procIsAdmin.Call()
	return r != 0
}

func elevate(args string) {
	exe, _ := os.Executable()
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exe)
	par, _ := syscall.UTF16PtrFromString(args)
	procShellExec.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), uintptr(unsafe.Pointer(par)), 0, 1)
}

func openPath(p string) {
	verb, _ := syscall.UTF16PtrFromString("open")
	file, _ := syscall.UTF16PtrFromString(p)
	procShellExec.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), 0, 0, 1)
}
