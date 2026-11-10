//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
)

func startTerminalPTY(cmd *exec.Cmd, cols, rows uint16) (*os.File, error) {
	return nil, errors.New("integrated terminals currently support macOS and Unix")
}
func resizeTerminalPTY(f *os.File, cols, rows uint16) error {
	return errors.New("terminal unavailable")
}
func stopTerminalPTY(cmd *exec.Cmd, f *os.File) { _ = cmd.Process.Kill(); _ = f.Close() }
