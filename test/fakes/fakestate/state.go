// Package fakestate is the on-disk state of the fake container CLIs in
// test/fakes/fakectr. Tests read it to assert what a backend asked the CLI to
// do (flags, mounts, networks) without parsing argv themselves.
package fakestate

import (
	"encoding/json"
	"errors"
	"os"
)

// Mount is one bind mount as the fake understood it.
type Mount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readonly"`
	Via      string `json:"via"` // "-v" or "--mount"
}

// Container is one instance (cage or keeper).
type Container struct {
	ID          string            `json:"id"` // apple: the name; docker: 64 hex chars
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	Cmd         []string          `json:"cmd,omitempty"`
	Running     bool              `json:"running"`
	Started     bool              `json:"started"` // ever started (docker "created" vs "exited")
	Labels      map[string]string `json:"labels"`
	Env         []string          `json:"env,omitempty"`
	Networks    []string          `json:"networks"`
	IPs         map[string]string `json:"ips"`     // current address per network ("" when stopped)
	LastIPs     map[string]string `json:"lastIps"` // docker reuses these on restart
	Mounts      []Mount           `json:"mounts,omitempty"`
	CPUs        float64           `json:"cpus"`
	Memory      int64             `json:"memory"`    // bytes
	PidsLimit   int               `json:"pidsLimit"` // docker --pids-limit
	Ulimits     []string          `json:"ulimits"`   // apple --ulimit
	CapDrop     []string          `json:"capDrop"`   // as given
	CapAdd      []string          `json:"capAdd"`    // as given
	SecurityOpt []string          `json:"securityOpt"`
	DNS         []string          `json:"dns"`                  // docker --dns
	NoDNS       bool              `json:"noDns"`                // apple --no-dns
	ExtraHosts  []string          `json:"extraHosts,omitempty"` // docker --add-host
	Runtime     string            `json:"runtime,omitempty"`
	Restart     string            `json:"restart,omitempty"`
	ReadOnlyFS  bool              `json:"readOnlyFs,omitempty"`
	Tmpfs       []string          `json:"tmpfs,omitempty"`
	User        string            `json:"user,omitempty"`
	Args        []string          `json:"args"` // the create/run argv
}

// Network is a network created through the fake (plus the built-in one).
type Network struct {
	Name     string            `json:"name"`
	Internal bool              `json:"internal"`
	Labels   map[string]string `json:"labels"`
	Options  map[string]string `json:"options,omitempty"` // driver options (-o k=v)
	Subnet   int               `json:"subnet"` // third octet of 192.168.X.0/24
	Next     int               `json:"next"`   // next host octet to hand out
}

// State is the whole fake runtime.
type State struct {
	Seq        int          `json:"seq"`
	Containers []*Container `json:"containers"`
	Networks   []*Network   `json:"networks"`
	Images     []string     `json:"images"`
	Calls      [][]string   `json:"calls"` // every invocation, argv[0] is the dialect
}

// Load reads the state file; a missing file is an empty state.
func Load(path string) (*State, error) {
	st := &State{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return st, nil
	}
	return st, json.Unmarshal(b, st)
}

// Save writes the state file atomically.
func (s *State) Save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Find returns the container whose name, id, or id prefix (>= 4 chars) is ref.
func (s *State) Find(ref string) *Container {
	for _, c := range s.Containers {
		if c.Name == ref || c.ID == ref {
			return c
		}
	}
	if len(ref) >= 4 {
		for _, c := range s.Containers {
			if len(c.ID) > len(ref) && c.ID[:len(ref)] == ref {
				return c
			}
		}
	}
	return nil
}

// Net returns the named network or nil.
func (s *State) Net(name string) *Network {
	for _, n := range s.Networks {
		if n.Name == name {
			return n
		}
	}
	return nil
}
