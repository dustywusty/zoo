//go:build windows

package proc

import (
	"os/exec"
	"testing"
)

const detachedProcess = 0x00000008

// The daemon must get a hidden console, never none: with DETACHED_PROCESS
// every console program it ran got a console of its own, which Windows
// Terminal showed as a window, every few seconds (D25).
func TestDetachHidesInsteadOfDetaching(t *testing.T) {
	cmd := exec.Command("zoo", "daemon")
	Detach(cmd)
	f := cmd.SysProcAttr.CreationFlags
	if f&createNoWindow == 0 || !cmd.SysProcAttr.HideWindow {
		t.Errorf("daemon not started with CREATE_NO_WINDOW: %#x", f)
	}
	if f&detachedProcess != 0 {
		t.Errorf("daemon started with DETACHED_PROCESS (%#x): its children would each open a window", f)
	}
}

func TestQuietKeepsOtherFlags(t *testing.T) {
	cmd := exec.Command("git", "status")
	Quiet(cmd)
	Quiet(cmd)
	if cmd.SysProcAttr.CreationFlags != createNoWindow || !cmd.SysProcAttr.HideWindow {
		t.Errorf("quiet: %#x", cmd.SysProcAttr.CreationFlags)
	}
}
