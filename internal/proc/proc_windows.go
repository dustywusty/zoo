//go:build windows

package proc

import (
	"os"
	"os/exec"
	"syscall"
)

const (
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
	queryLimitedInfo      = 0x1000 // PROCESS_QUERY_LIMITED_INFORMATION
	stillActive           = 259
)

// Detach starts cmd in the background with a console that has no window.
//
// Not DETACHED_PROCESS: a process with no console at all makes Windows give
// every console program it starts (git, docker) a console of its own, and
// with Windows Terminal as the default terminal each one opens a window.
// A daemon that runs commands every few seconds then floods the desktop.
// With CREATE_NO_WINDOW the daemon has a hidden console its children
// inherit, so nothing it runs can show a window.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | createNoWindow, HideWindow: true}
}

// Quiet keeps a command whose output zoo captures from ever showing a
// window, whoever starts it. Don't use it for commands wired to the user's
// terminal (zoo run, zoo shell): they need the caller's console.
func Quiet(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
	cmd.SysProcAttr.HideWindow = true
}

// Terminate stops pid. A hidden-console process gets no SIGTERM on Windows;
// the daemon holds no state that needs a graceful exit.
func Terminate(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// Alive reports whether pid is a running process.
func Alive(pid int) bool {
	h, err := syscall.OpenProcess(queryLimitedInfo, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	return syscall.GetExitCodeProcess(h, &code) == nil && code == stillActive
}
