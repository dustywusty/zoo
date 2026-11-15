package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/dusty/zoo/internal/shims"
	"github.com/dusty/zoo/internal/update"
)

// cmdUpdate: `zoo update` replaces zoo with the latest release. `--check`
// only says whether one exists. Homebrew and package installs get the
// command to run instead, so their package manager stays right.
func cmdUpdate(args []string) error {
	check := false
	for _, a := range args {
		if a != "--check" {
			return errors.New("usage: zoo update [--check]")
		}
		check = true
	}
	g, err := loadCfg()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c := &http.Client{Timeout: 5 * time.Minute}

	st := update.Check(ctx, c, version, exe)
	_ = update.Save(st)
	if st.Error != "" {
		return errors.New(st.Error)
	}
	if !st.Available {
		say("zoo %s is up to date (latest release: %s)", version, st.Latest)
		return nil
	}
	say("zoo %s is available; you have %s (%s)", st.Latest, version, st.URL)
	if st.Kind != update.Self {
		say("zoo was installed with %s. To update, run: %s", st.Kind, st.Command)
		return nil
	}
	if check {
		say("to update, run: zoo update")
		return nil
	}

	r, err := update.Latest(ctx, c)
	if err != nil {
		return err
	}
	if err := update.Apply(ctx, c, r, exe); err != nil {
		return err
	}
	say("updated %s to %s", exe, r.Version())

	// The rest runs the new zoo by its path. On Windows, this process's
	// own path now names the old copy that Apply moved aside.
	if shims.Installed() {
		cmd := exec.Command(exe, "shims", "install")
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			say("shims not refreshed: %v (run `zoo shims install`)", err)
		}
	}
	if daemonAlive(g) {
		stopDaemon()
		time.Sleep(500 * time.Millisecond)
		if err := startDaemonFrom(g, exe); err != nil {
			say("dashboard not restarted: %v (run `zoo dash`)", err)
		} else {
			say("dashboard restarted on %s", r.Version())
		}
	}
	update.RemoveOld(exe) // copies that still run stay until the next update
	return nil
}
