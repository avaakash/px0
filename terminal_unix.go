//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

func startTerminalPTY(cmd *exec.Cmd, cols, rows uint16) (*os.File, error) {
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	// Darwin's PTY starts as an unpollable os.File. Wrap a nonblocking duplicate
	// so Go registers it with its poller; simply changing flags after NewFile
	// would make reads return EAGAIN and lose output, and Close could hang.
	fd, err := syscall.Dup(int(f.Fd()))
	if err == nil {
		syscall.CloseOnExec(fd)
		err = syscall.SetNonblock(fd, true)
	}
	_ = f.Close()
	if err != nil {
		if fd >= 0 {
			_ = syscall.Close(fd)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}
	return os.NewFile(uintptr(fd), "/dev/ptmx"), nil
}
func resizeTerminalPTY(f *os.File, cols, rows uint16) error {
	return pty.Setsize(f, &pty.Winsize{Cols: cols, Rows: rows})
}
func stopTerminalPTY(cmd *exec.Cmd, f *os.File) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP)
	_ = f.Close()
}
