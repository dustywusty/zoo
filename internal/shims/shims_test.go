package shims

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWithFirst(t *testing.T) {
	dir := `C:\Users\me\.zoo\shims`
	for _, c := range []struct{ cur, want string }{
		{`C:\bin;C:\tools`, dir + `;C:\bin;C:\tools`},
		{`C:\bin;c:\users\me\.zoo\shims\;C:\tools`, dir + `;C:\bin;C:\tools`}, // moved to the front, once
		{``, dir},
		{dir + `;C:\bin`, dir + `;C:\bin`},
	} {
		got, changed := withFirst(c.cur, dir)
		if got != c.want || changed != (c.cur != c.want) {
			t.Errorf("withFirst(%q) = %q, %v; want %q", c.cur, got, changed, c.want)
		}
	}
	if got := without(`C:\bin;`+dir+`;;C:\tools`, dir); got != `C:\bin;C:\tools` {
		t.Errorf("without = %q", got)
	}
}

// The real CLI is found past the shims dir, which must never win.
func TestFindRealSkipsShims(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ZOO_HOME", home)
	shimDir := filepath.Join(home, "shims")
	realDir := t.TempDir()
	name, ext := "fakeagent", ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
		t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	}
	for _, d := range []string{shimDir, realDir} {
		_ = os.MkdirAll(d, 0o755)
		if err := os.WriteFile(filepath.Join(d, name+ext), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+realDir)
	got, err := FindReal(name)
	if err != nil || filepath.Dir(got) != realDir {
		t.Fatalf("FindReal = %q, %v; want the one in %s", got, err, realDir)
	}
	t.Setenv("PATH", shimDir)
	if _, err := FindReal(name); err == nil {
		t.Fatal("found the shim itself")
	}
}

// Windows gets executable shims (Node won't spawn .cmd), with a manifest
// pointing at the real zoo; uninstall removes exactly those.
func TestInstallExeShims(t *testing.T) {
	if !exeShims {
		t.Skip("executable shims are for Windows")
	}
	home := t.TempDir()
	t.Setenv("ZOO_HOME", home)
	zoo := filepath.Join(t.TempDir(), "zoo.exe")
	if err := os.WriteFile(zoo, []byte("MZ fake zoo"), 0o755); err != nil {
		t.Fatal(err)
	}
	written, err := Install([]string{"codex", "shell", "claude"}, zoo)
	if err != nil || len(written) != 2 {
		t.Fatalf("install: %v %v", written, err)
	}
	for _, h := range []string{"claude", "codex"} {
		if b, err := os.ReadFile(filepath.Join(home, "shims", h+".exe")); err != nil || string(b) != "MZ fake zoo" {
			t.Errorf("%s.exe: %q %v", h, b, err)
		}
	}
	if z, names := readManifest(); z != zoo || len(names) != 2 {
		t.Errorf("manifest: %q %v", z, names)
	}
	_ = os.WriteFile(filepath.Join(home, "shims", "mine.exe"), []byte("not ours"), 0o755)
	removed, _ := Uninstall()
	if len(removed) != 2 {
		t.Errorf("removed %v", removed)
	}
	if _, err := os.Stat(filepath.Join(home, "shims", "mine.exe")); err != nil {
		t.Error("uninstall removed a file zoo didn't write")
	}
}

// Upgrading zoo on Windows while the zoo daemon ran left an old shim that
// could not be replaced: Windows refuses to delete or overwrite a running
// exe. Here a shim runs (a copy of ping.exe that waits), and a new install
// must still put the new shim in place.
func TestInstallExeReplacesARunningShim(t *testing.T) {
	if !exeShims {
		t.Skip("executable shims are for Windows")
	}
	ping, err := exec.LookPath("ping.exe")
	if err != nil {
		t.Skip(err)
	}
	home := t.TempDir()
	t.Setenv("ZOO_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "shims"), 0o755); err != nil {
		t.Fatal(err)
	}
	running := filepath.Join(home, "shims", "claude.exe")
	if err := copyFile(ping, running); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(running, "-n", "30", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	zoo := filepath.Join(t.TempDir(), "zoo.exe")
	if err := os.WriteFile(zoo, []byte("MZ new zoo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install([]string{"claude"}, zoo); err != nil {
		t.Fatalf("install over a running shim: %v", err)
	}
	if b, err := os.ReadFile(running); err != nil || string(b) != "MZ new zoo" {
		t.Errorf("claude.exe after install: %q %v", b, err)
	}

	// Once the old shim stops, the next install deletes it.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if _, err := Install([]string{"claude"}, zoo); err != nil {
		t.Fatal(err)
	}
	if old, _ := filepath.Glob(filepath.Join(home, "shims", "*.old-*")); len(old) != 0 {
		t.Errorf("old shims left after they stopped: %v", old)
	}
}
