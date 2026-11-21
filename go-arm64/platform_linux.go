//go:build linux

package main

import (
	"io"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const isWindows = false

func deviceSize(f *os.File) (int64, error) {
	if st, err := f.Stat(); err == nil && st.Mode().IsRegular() {
		return st.Size(), nil
	}
	var size uint64
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0x80081272, uintptr(unsafe.Pointer(&size))); errno == 0 {
		return int64(size), nil
	}
	return f.Seek(0, io.SeekEnd)
}

func hideCmd(c *exec.Cmd) {}
func isAdmin() bool       { return os.Geteuid() == 0 }
func elevate(args string) {}
func openPath(p string)   {}
