//go:build !windows

package proc

import (
	"os"
	"os/exec"
	"syscall"
)

// Detach makes cmd outlive the terminal that started it.
func Detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// Quiet is a no-op off Windows: child processes there never get windows.
func Quiet(*exec.Cmd) {}

// Terminate asks pid to exit.
func Terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGTERM)
}

// Alive reports whether pid is a running process.
func Alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
