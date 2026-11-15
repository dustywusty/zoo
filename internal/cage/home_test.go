package cage

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/dusty/zoo/internal/backend"
	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/paths"
)

func mountsByTarget(spec backend.CageSpec) map[string]backend.Mount {
	out := map[string]backend.Mount{}
	for _, m := range spec.Mounts {
		out[m.Target] = m
	}
	return out
}

// The agent's chat history and settings survive a recreate: each kept
// folder is a host folder of that cage, mounted read-write.
func TestKeptHomeIsMountedPerCage(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	root := t.TempDir()
	pl, err := NewPlan(root, config.Defaults(), config.Project{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if pl.HomeDir != paths.AgentHome(pl.Name) {
		t.Errorf("HomeDir = %q, want %q", pl.HomeDir, paths.AgentHome(pl.Name))
	}
	m := mountsByTarget(pl.Spec(config.Defaults(), "10.0.0.2", nil))
	for _, d := range KeptHome {
		got, ok := m[AgentHomeTarget+"/"+d]
		if !ok || got.ReadOnly || got.Source != filepath.Join(pl.HomeDir, d) {
			t.Errorf("%s: mount %+v (found %v)", d, got, ok)
		}
		if st, err := os.Stat(got.Source); err != nil || !st.IsDir() {
			t.Errorf("%s: host folder not created: %v", d, err)
		}
	}
}

// host_folders: every listed folder that exists is mounted at its own
// path, read-only when asked. A missing folder is left out, and a
// relative one is an error.
func TestHostFolders(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	root := t.TempDir()
	scratch, attach := t.TempDir(), t.TempDir()
	g := config.Defaults()
	g.HostFolders = []config.HostFolder{
		{Path: scratch},
		{Path: attach, ReadOnly: true},
		{Path: filepath.Join(scratch, "does-not-exist")},
	}
	pl, err := NewPlan(root, g, config.Project{}, "")
	if err != nil {
		t.Fatal(err)
	}
	m := mountsByTarget(pl.Spec(g, "10.0.0.2", nil))
	if got, ok := m[scratch]; !ok || got.ReadOnly || got.Source != scratch {
		t.Errorf("scratch: %+v %v", got, ok)
	}
	if got, ok := m[attach]; !ok || !got.ReadOnly {
		t.Errorf("attachments: %+v %v, want read-only", got, ok)
	}
	if _, ok := m[filepath.Join(scratch, "does-not-exist")]; ok {
		t.Error("a missing folder was mounted")
	}
	if got, want := pl.GuestDirs(), []string{root, scratch, attach}; !reflect.DeepEqual(got, want) {
		t.Errorf("GuestDirs = %v, want %v", got, want)
	}

	g.HostFolders = []config.HostFolder{{Path: "relative/dir"}}
	if _, err := NewPlan(root, g, config.Project{}, ""); err == nil {
		t.Error("a relative host folder was accepted")
	}
}

// Paths in an agent's arguments that the cage sees are rewritten to the
// cage's paths; everything else stays as it is.
func TestGuestArgs(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "chat", "1")
	out := filepath.Join(t.TempDir(), "elsewhere")
	args := []string{"--add-dir=" + in, "--plugin-dir", in, "--add-dir=" + out, "-p", "hello " + in, "--model=x", "relative/path"}
	got := GuestArgs(args, []string{dir})
	want := []string{"--add-dir=" + backend.GuestPath(in), "--plugin-dir", backend.GuestPath(in), "--add-dir=" + out, "-p", "hello " + in, "--model=x", "relative/path"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GuestArgs\n got %q\nwant %q", got, want)
	}
	if runtime.GOOS == "windows" && got[0] == args[0] {
		t.Errorf("on Windows, %q was not rewritten", args[0])
	}
}
