package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/paths"
	"github.com/dusty/zoo/internal/task"
	"github.com/dusty/zoo/test/fakes/fakestate"
)

func TestTaskLifecycleWithFakeRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake runtime uses a Unix shell")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "docker")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/dusty/zoo/test/fakes/fakectr").CombinedOutput(); err != nil {
		t.Fatalf("build fake: %v: %s", err, out)
	}
	home := filepath.Join(tmp, "home")
	state := filepath.Join(tmp, "state.json")
	t.Setenv("ZOO_HOME", home)
	t.Setenv("ZOO_KEYS_FILE", "1")
	t.Setenv("ZOO_DOCKER_BIN", bin)
	t.Setenv("ZOO_DOCKER_API", "off")
	t.Setenv("ZOO_FAKE_STATE", state)
	t.Setenv("ZOO_FAKE_DIALECT", "docker")
	t.Setenv("ZOO_FAKE_DOWN", "")
	t.Setenv("ZOO_FAKE_NETWORK_CREATE_FAIL", "")
	t.Setenv("ZOO_FAKE_NO_EXEC", "1")
	t.Setenv("ZOO_BACKEND", "docker")
	oldBackend, oldFloor := flagBackend, flagFloor
	flagBackend, flagFloor = "docker", "container"
	defer func() { flagBackend, flagFloor = oldBackend, oldFloor }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) }))
	defer server.Close()
	port := server.Listener.Addr().(*net.TCPAddr).Port
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(fmt.Sprintf("port: %d\nbackend: docker\nisolation_floor: container\n", port)), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(tmp, "project")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	source, _ = filepath.EvalSymlinks(source)
	original := filepath.Join(source, "file.txt")
	if err := os.WriteFile(original, []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cmdTask([]string{"new", "review", source}); err != nil {
		t.Fatal(err)
	}
	saved, err := task.Load("review")
	if err != nil {
		t.Fatal(err)
	}
	if err := cmdTask([]string{"run", "review", "shell"}); err != nil {
		t.Fatal(err)
	}
	st, err := fakestate.Load(state)
	if err != nil {
		t.Fatal(err)
	}
	c := st.Find(cage.Prefix + saved.Cage())
	if c == nil || c.Running {
		t.Fatal("task cage missing or not stopped on exit")
	}
	foundWork := false
	for _, m := range c.Mounts {
		if m.Source == source || m.Source == saved.Dir() || m.Source == filepath.Join(saved.Dir(), "review.git") {
			t.Fatalf("host source or review metadata exposed: %+v", m)
		}
		if m.Source == saved.Workdir() && !m.ReadOnly {
			foundWork = true
		}
	}
	if !foundWork {
		t.Fatal("no writable task mount")
	}
	c.Running = true
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	if err := cmdTask([]string{"apply", "review"}); err == nil {
		t.Fatal("apply accepted a running task cage")
	}
	c.Running = false
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(saved.Workdir(), "file.txt"), []byte("edited\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cmdTask([]string{"destroy", "review"}); err == nil {
		t.Fatal("destroy discarded edits")
	}
	if err := cmdTask([]string{"apply", "review", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(original)
	if string(b) != "original\n" {
		t.Fatal("dry run changed host")
	}
	if err := cmdTask([]string{"apply", "review"}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(original)
	if string(b) != "edited\n" {
		t.Fatal("apply did not change host")
	}
	if err := cmdTask([]string{"destroy", "review"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.CacheProject(saved.Cage())); !os.IsNotExist(err) {
		t.Fatalf("task cache survived: %v", err)
	}
	if _, err := task.Load("review"); err == nil {
		t.Fatal("task survived destroy")
	}
	st, err = fakestate.Load(state)
	if err != nil || st.Find(cage.Prefix+saved.Cage()) != nil {
		t.Fatalf("runtime cage survived destroy: %v", err)
	}
}
