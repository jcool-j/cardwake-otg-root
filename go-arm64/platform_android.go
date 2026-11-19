//go:build android || linux

package main

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const isWindows = false
const blkGetSize64 = 0x80081272

func deviceSize(f *os.File) (int64, error) {
	if st, err := f.Stat(); err == nil && st.Mode().IsRegular() {
		return st.Size(), nil
	}
	var size uint64
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(blkGetSize64), uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		return 0, errno
	}
	return int64(size), nil
}

func hideCmd(c *exec.Cmd) {}
func isAdmin() bool       { return os.Geteuid() == 0 }
func elevate(args string) {}
func openPath(p string)   {}
