package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/backend"
)

// Verbatim from `docker stats --no-stream --format '{{json .}}'` on OrbStack 29.8.2.
const statsLines = `{"BlockIO":"221kB / 0B","CPUPerc":"1.50%","Container":"2891f9d70850","ID":"2891f9d70850","MemPerc":"0.10%","MemUsage":"2.148MiB / 2GiB","Name":"zoo-repo-wt-e1a975","NetIO":"16.7kB / 4.28kB","PIDs":"2"}
{"BlockIO":"0B / 0B","CPUPerc":"0.00%","Container":"abc","ID":"abc","MemPerc":"0.00%","MemUsage":"480KiB / 15.66GiB","Name":"someone-elses","NetIO":"0B / 0B","PIDs":"1"}`

func TestParseStats(t *testing.T) {
	got := parseStats(statsLines)
	if len(got) != 1 {
		t.Fatalf("want only zoo containers: %+v", got)
	}
	s := got[0]
	if s.ID != "zoo-repo-wt-e1a975" || s.CPUPercent != 1.5 || s.MemLimit != 2<<30 || s.NetRx != 16700 || s.NetTx != 4280 || s.BlkRead != 221000 || s.Pids != 2 {
		t.Fatalf("%+v", s)
	}
	if want := 2.148 * (1 << 20); s.MemUsed != want {
		t.Fatalf("mem used %v", s.MemUsed)
	}
	// podman prints an array with snake_case keys.
	pod := parseStats(`[{"name":"zoo-p","cpu_percent":"3.00%","mem_usage":"1MB / 2GB","net_io":"1kB / 2kB","block_io":"0B / 0B","pids":"4"}]`)
	if len(pod) != 1 || pod[0].CPUPercent != 3 || pod[0].MemLimit != 2e9 || pod[0].Pids != 4 {
		t.Fatalf("podman: %+v", pod)
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]float64{"648B": 648, "1.2kB": 1200, "1.5MiB": 1.5 * (1 << 20), "2GiB": 2 << 30, "0B": 0, " 3.97MB ": 3.97e6} {
		if got, ok := ParseBytes(in); !ok || got != want {
			t.Errorf("%q: got %v %v, want %v", in, got, ok, want)
		}
	}
	if _, ok := ParseBytes("--"); ok {
		t.Error("garbage should not parse")
	}
}

func TestMountArgQuotesCommas(t *testing.T) {
	if got := mountArg("/a,b", "/x", true); got != `type=bind,"source=/a,b",target=/x,readonly` {
		t.Fatal(got)
	}
	if got := mountArg(`/q"`, "/x", false); got != `type=bind,"source=/q""",target=/x` {
		t.Fatal(got)
	}
}

func TestIsolationFor(t *testing.T) {
	for rt, want := range map[string]backend.Isolation{
		"runc": backend.IsoContainer, "": backend.IsoContainer, "io.containerd.runc.v2": backend.IsoContainer,
		"runsc": backend.IsoSandboxed, "kata": backend.IsoMicroVM, "io.containerd.kata.v2": backend.IsoMicroVM,
	} {
		if got := isolationFor(rt); got != want {
			t.Errorf("%q: %s, want %s", rt, got, want)
		}
	}
}

func TestKeeperArgs(t *testing.T) {
	got := strings.Join(KeeperArgs(backend.KeeperSpec{
		ID: "zoo-keeper", Image: "zoo-keeper:1", CPUs: 1, Memory: "256M",
		EtcDir: "/z/etc", LogDir: "/z/log", Labels: map[string]string{"zoo.role": "keeper"},
	}), " ")
	for _, want := range []string{
		"create --name zoo-keeper", "--network " + EgressNetwork, "--cap-drop ALL", "--security-opt no-new-privileges",
		// Reverse-path filtering drops forged cage source addresses in the kernel.
		"--sysctl net.ipv4.conf.all.rp_filter=1", "--sysctl net.ipv4.conf.default.rp_filter=1",
		"--label zoo.role=keeper", "--mount type=bind,source=/z/etc,target=/etc/zoo ",
	} {
		if !strings.Contains(got+" ", want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, " zoo-keeper:1") {
		t.Errorf("image must come last:\n%s", got)
	}
	if strings.Contains(got, "NET_ADMIN") {
		t.Errorf("the keeper must not need NET_ADMIN for its sysctls:\n%s", got)
	}
}

func TestRunArgs(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	git := filepath.Join(root, ".git")
	_ = os.MkdirAll(filepath.Join(git, "hooks"), 0o755)
	_ = os.WriteFile(filepath.Join(git, "config"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1"), 0o644)
	d := New("docker", backend.Options{Network: "zoo-cages", MaskFile: "/m/empty", Runtime: "runsc"})
	got := strings.Join(d.RunArgs(backend.CageSpec{
		ID: "zoo-repo-1", Image: "zoo-cage:latest", CPUs: 2, Memory: "2G",
		Labels:   map[string]string{"zoo.cage": "repo-1"},
		Mounts:   []backend.Mount{{Source: root, Target: root}},
		Masks:    []string{filepath.Join(root, ".env")},
		ReadOnly: []string{filepath.Join(git, "hooks"), filepath.Join(git, "config"), filepath.Join(git, "nope")},
		Pin:      []string{git},
	}), " ")
	for _, want := range []string{
		"--network zoo-cages", "--dns 127.0.0.1", "--cpus 2", "--memory 2G", "--pids-limit 1024",
		"--cap-drop ALL", "--security-opt no-new-privileges", "--runtime runsc", "--label zoo.cage=repo-1",
		"--env ZOO_GUARD=host",
		"--mount type=bind,source=" + root + ",target=" + root + " ",
		"--mount type=bind,source=" + git + ",target=" + git + " ",
		"--mount type=bind,source=" + filepath.Join(git, "config") + ",target=" + filepath.Join(git, "config") + ",readonly",
		"--mount type=bind,source=/m/empty,target=" + filepath.Join(root, ".env") + ",readonly",
	} {
		if !strings.Contains(got+" ", want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "nope") || strings.Contains(got, "docker.sock") {
		t.Errorf("unexpected mount:\n%s", got)
	}
}
