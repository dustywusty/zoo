package apple

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/backend"
)

// Verbatim from `container stats --no-stream --format json` on 1.6.1.
const stats161 = `[{"blockReadBytes":1683456,"blockWriteBytes":52428800,"cpuUsageUsec":28823,"id":"zoo-probe-a",
  "memoryLimitBytes":1073741824,"memoryUsageBytes":56221696,"networkRxBytes":36304,
  "networkTxBytes":1204,"numProcesses":1}]`

func TestParseStatExact(t *testing.T) {
	var arr []any
	if err := json.Unmarshal([]byte(stats161), &arr); err != nil {
		t.Fatal(err)
	}
	s := parseStat(arr[0])
	if s.ID != "zoo-probe-a" || s.CPUTimeNs != 28823e3 || s.MemUsed != 56221696 || s.MemLimit != 1073741824 ||
		s.NetRx != 36304 || s.NetTx != 1204 || s.BlkRead != 1683456 || s.BlkWrite != 52428800 || s.Pids != 1 {
		t.Fatalf("%+v", s)
	}
	if !s.Have["cpu_time"] || s.Have["cpu"] || !s.Have["mem"] || !s.Have["net"] || !s.Have["blk"] || !s.Have["pids"] {
		t.Fatalf("have: %v", s.Have)
	}
}

func TestParseStatHeuristicFallback(t *testing.T) {
	var el any
	_ = json.Unmarshal([]byte(`{"id":"zoo-x","cpu":{"usageNanos":5000},"memory":{"usedBytes":10,"limitBytes":20}}`), &el)
	s := parseStat(el)
	if s.ID != "zoo-x" || s.CPUTimeNs != 5000 || s.MemUsed != 10 || s.MemLimit != 20 {
		t.Fatalf("%+v", s)
	}
}

func TestRunArgs(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	git := filepath.Join(root, ".git")
	_ = os.MkdirAll(filepath.Join(git, "hooks"), 0o755)
	_ = os.WriteFile(filepath.Join(git, "config"), []byte("[core]\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1"), 0o644)
	mask := filepath.Join(dir, "mask", "empty")
	_ = os.MkdirAll(filepath.Dir(mask), 0o755)
	_ = os.WriteFile(mask, nil, 0o444)
	stage := filepath.Join(dir, "stage")

	a := New(backend.Options{Network: "zoo-cages", MaskFile: mask, StageDir: stage})
	args, err := a.RunArgs(backend.CageSpec{
		ID: "zoo-repo-123456", Image: "zoo-cage:latest", CPUs: 2, Memory: "2G", Pids: 1024,
		Labels:   map[string]string{"zoo.cage": "repo-123456"},
		Env:      []string{"HTTPS_PROXY=http://192.168.128.2:3128"},
		Mounts:   []backend.Mount{{Source: root, Target: root}},
		Masks:    []string{filepath.Join(root, ".env")},
		ReadOnly: []string{filepath.Join(git, "hooks"), filepath.Join(git, "config"), filepath.Join(git, "missing")},
		Pin:      []string{git},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	staged := filepath.Join(stage, "zoo-repo-123456", "1", "config")
	for _, want := range []string{
		"--network zoo-cages", "--no-dns", "-c 2", "-m 2G", "--cap-drop ALL", "--ulimit nproc=1024:1024",
		"-l zoo.cage=repo-123456", "-e ZOO_GUARD=host",
		"-v " + root + ":" + root + " ",
		"-v " + git + ":" + git + " ", // pin
		"-v " + filepath.Join(git, "hooks") + ":" + filepath.Join(git, "hooks") + ":ro",
		// A file in a pinned dir is mounted from a staged copy, else the
		// runtime drops the pin.
		"-v " + staged + ":" + filepath.Join(git, "config") + ":ro",
		"-v " + mask + ":" + filepath.Join(root, ".env") + ":ro",
	} {
		if !strings.Contains(got+" ", want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "missing") {
		t.Error("nonexistent read-only paths must be skipped")
	}
	if b, err := os.ReadFile(staged); err != nil || string(b) != "[core]\n" {
		t.Errorf("staged copy: %q %v", b, err)
	}
	if !strings.HasSuffix(got, "zoo-cage:latest") {
		t.Error("image must be last")
	}
}

func TestRunArgsRejectsColon(t *testing.T) {
	a := New(backend.Options{Network: "n"})
	if _, err := a.RunArgs(backend.CageSpec{ID: "zoo-x", Mounts: []backend.Mount{{Source: "/a:b", Target: "/a:b"}}}); err == nil {
		t.Fatal("paths with ':' can't be expressed with -v and must be refused, not mangled")
	}
}
