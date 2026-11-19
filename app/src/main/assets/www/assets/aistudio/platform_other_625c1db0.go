//go:build !windows

package main

import (
	"io"
	"os"
	"os/exec"
)

const isWindows = false

func deviceSize(f *os.File) (int64, error) {
	if st, err := f.Stat(); err == nil && st.Mode().IsRegular() {
		return st.Size(), nil
	}
	return f.Seek(0, io.SeekEnd)
}

func hideCmd(c *exec.Cmd) {}
func isAdmin() bool       { return os.Geteuid() == 0 }
func elevate(args string) {}
func openPath(p string)   {}
