// Package backend is the line between zoo and whatever runs cages: Apple
// `container` VMs, Docker/Podman containers, and later k8s, Lima, Firecracker.
// Everything runtime-specific lives in a subpackage (backend/apple,
// backend/docker) that registers itself here; the rest of zoo only sees this
// interface.
package backend

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Isolation is how strongly a cage is separated from the host and other cages.
type Isolation string

const (
	IsoVM        Isolation = "vm"                  // own kernel, full VM
	IsoMicroVM   Isolation = "microvm"             // own kernel, minimal VMM (Kata, Firecracker)
	IsoSandboxed Isolation = "sandboxed-container" // gVisor
	IsoContainer Isolation = "container"           // shared kernel
)

// rank orders isolation levels; a VM and a microVM both give a cage its own kernel.
var rank = map[Isolation]int{IsoContainer: 1, IsoSandboxed: 2, IsoMicroVM: 3, IsoVM: 3}

// ParseIsolation accepts the names above ("" and "auto" are not valid here).
func ParseIsolation(s string) (Isolation, error) {
	i := Isolation(strings.ToLower(strings.TrimSpace(s)))
	if _, ok := rank[i]; !ok {
		return "", fmt.Errorf("unknown isolation %q (vm, microvm, sandboxed-container, container)", s)
	}
	return i, nil
}

// AtLeast reports whether i is as strong as floor.
func (i Isolation) AtLeast(floor Isolation) bool { return rank[i] >= rank[floor] }

// Capabilities is what a backend can do on this host.
type Capabilities struct {
	Isolation            Isolation `json:"isolation"`
	InternalNetworks     bool      `json:"internal_networks"`      // a network with no route out
	HostGatewayReachable bool      `json:"host_gateway_reachable"` // cages can reach the host on that network
	MultiNetworkAttach   bool      `json:"multi_network_attach"`   // keeper dual-homing
	HostFileMounts       bool      `json:"host_file_mounts"`       // bind one host file (host-side masking)
	ReadOnlyMounts       bool      `json:"read_only_mounts"`
	GuestMountCap        bool      `json:"guest_mount_cap"` // guest root can bind/remount (guest-side masking)
	PidsLimit            bool      `json:"pids_limit"`
	DiskLimit            bool      `json:"disk_limit"`
	PerCageKernel        bool      `json:"per_cage_kernel"`
}

// Info is what Detect found.
type Info struct {
	Name      string       `json:"name"`
	Installed bool         `json:"installed"`
	Running   bool         `json:"running"`
	Version   string       `json:"version,omitempty"`
	Detail    string       `json:"detail,omitempty"` // e.g. "OrbStack", "Docker Desktop", "podman"
	Hint      string       `json:"hint,omitempty"`   // what to do when not installed/running
	Caps      Capabilities `json:"caps"`
}

// Ready reports whether the backend can create cages right now.
func (i Info) Ready() bool { return i.Installed && i.Running }

// Mount is a host path made visible in a cage.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// CageSpec is everything a backend needs to create a cage.
type CageSpec struct {
	ID       string
	Image    string
	CPUs     int
	Memory   string // "2G"
	Pids     int    // 0 = backend default
	Labels   map[string]string
	Env      []string // non-secret only (K=V)
	Mounts   []Mount  // rw mounts of the worktree and git dir, same path inside
	Masks    []string // files to show as empty and read-only
	ReadOnly []string // paths to make read-only (.git/hooks, .git/config)
	Pin      []string // directories that must not be renamed from inside (.git)
}

// KeeperSpec describes the keeper instance.
type KeeperSpec struct {
	ID     string
	Image  string
	EtcDir string // host dir mounted at /etc/zoo
	LogDir string // host dir mounted at /var/log/zoo
	CPUs   int
	Memory string
	Labels map[string]string
}

// ImageSpec is an image build.
type ImageSpec struct {
	Tag       string
	Context   string
	BuildArgs map[string]string
}

// Status is one instance (cage or keeper) as the backend sees it.
type Status struct {
	ID      string            `json:"id"`
	State   string            `json:"state"`
	Running bool              `json:"running"`
	Labels  map[string]string `json:"labels"`
	IP      string            `json:"ip"` // address on the cage network
}

// ExecSpec runs a command in an instance. Nil Stdin/Stdout/Stderr mean the
// caller's terminal.
type ExecSpec struct {
	ID      string
	Cmd     []string
	User    string
	Workdir string
	Env     []string // non-secret K=V
	EnvFile string   // 0600 file with secrets; read by the runtime CLI, never put on argv
	TTY     bool
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	NoStdin bool // don't attach stdin at all
}

// Sample is one resource reading. Counters are cumulative; the daemon turns
// them into rates.
type Sample struct {
	ID         string             `json:"id"`
	CPUPercent float64            `json:"cpu_percent"` // of one core, when the backend reports it
	CPUTimeNs  float64            `json:"cpu_time_ns"` // cumulative, when it reports that instead
	MemUsed    float64            `json:"mem_used"`
	MemLimit   float64            `json:"mem_limit"`
	NetRx      float64            `json:"net_rx"`
	NetTx      float64            `json:"net_tx"`
	BlkRead    float64            `json:"blk_read"`
	BlkWrite   float64            `json:"blk_write"`
	Pids       float64            `json:"pids"`
	Have       map[string]bool    `json:"have"` // cpu, cpu_time, mem, net, blk, pids
	Raw        map[string]float64 `json:"raw,omitempty"`
}

// Backend creates and runs cages and the keeper.
type Backend interface {
	Name() string
	Detect(ctx context.Context) Info
	// Prepare starts the runtime's services if it can (may be interactive).
	Prepare(ctx context.Context) error

	ImageExists(ctx context.Context, ref string) (bool, error)
	BuildImage(ctx context.Context, spec ImageSpec) error

	// EnsureNetwork creates the internal cage network if missing.
	EnsureNetwork(ctx context.Context) error
	// EnsureKeeper creates or starts the keeper, attached to the cage network
	// and a network with a route out, and returns its IP on the cage network.
	EnsureKeeper(ctx context.Context, spec KeeperSpec) (string, error)

	CreateCage(ctx context.Context, spec CageSpec) error
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	// List returns zoo's instances (cages and keeper), running or not.
	List(ctx context.Context) ([]Status, error)
	Get(ctx context.Context, id string) (Status, bool, error)

	// Exec runs a command and returns its exit code. err is non-nil only when
	// the command couldn't be run at all.
	Exec(ctx context.Context, spec ExecSpec) (int, error)
	// Sample reads resources for running instances.
	Sample(ctx context.Context) ([]Sample, error)
}

// Options configure a backend instance.
type Options struct {
	Network  string // internal cage network name
	Bin      string // CLI override ("" = backend default)
	Runtime  string // docker: "" | runc | runsc | kata (OCI runtime name)
	MaskFile string // empty read-only host file used for host-side masks
	StageDir string // where backends may stage per-cage files (apple)
}

// Factory builds a backend.
type Factory func(Options) Backend

var factories = map[string]Factory{}

// Register makes a backend available by name. Called from backend packages' init.
func Register(name string, f Factory) { factories[name] = f }

// Names lists registered backends in preference order for auto-detection.
func Names() []string {
	order := map[string]int{"apple": 0, "docker": 1, "podman": 2}
	out := make([]string, 0, len(factories))
	for n := range factories {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		oi, iok := order[out[i]]
		oj, jok := order[out[j]]
		if iok && jok {
			return oi < oj
		}
		if iok != jok {
			return iok
		}
		return out[i] < out[j]
	})
	return out
}

// New builds a named backend.
func New(name string, o Options) (Backend, error) {
	f, ok := factories[name]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (have: %s)", name, strings.Join(Names(), ", "))
	}
	return f(o), nil
}

// Auto picks the strongest ready backend; ties go to preference order. If
// none is ready it returns the strongest installed one so the caller can
// explain what to start.
func Auto(ctx context.Context, o Options) (Backend, Info, error) {
	var best Backend
	var bestInfo Info
	score := func(i Info) int {
		s := rank[i.Caps.Isolation]
		if i.Installed {
			s += 10
		}
		if i.Running {
			s += 100
		}
		return s
	}
	for _, n := range Names() {
		b, _ := New(n, o)
		info := b.Detect(ctx)
		if !info.Installed {
			continue
		}
		if best == nil || score(info) > score(bestInfo) {
			best, bestInfo = b, info
		}
	}
	if best == nil {
		return nil, Info{}, fmt.Errorf("no container runtime found: install Apple's `container` (macOS 26+, https://github.com/apple/container/releases), OrbStack, Docker, or Podman")
	}
	return best, bestInfo, nil
}

// Prefixed reports whether id looks like a zoo instance.
func Prefixed(id string) bool { return strings.HasPrefix(id, "zoo-") }

// RunningState reports whether a runtime's state string means running.
func RunningState(s string) bool {
	s = strings.ToLower(s)
	return s == "running" || strings.HasPrefix(s, "up")
}

// SortedKeys returns m's keys in order, for deterministic argv.
func SortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Existing filters paths to those present on the host. Mount sources must
// exist; a missing .git/hooks would otherwise be created by some runtimes.
func Existing(paths []string) []string {
	var out []string
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}
