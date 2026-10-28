// Package ctr drives Apple's `container` CLI (github.com/apple/container).
// Every call shells out, so zoo works with whatever version is installed;
// JSON output is read loosely through jsonx.
package ctr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/dusty/zoo/internal/jsonx"
)

// Bin is the container CLI. Override with ZOO_CONTAINER_BIN (used by tests).
func Bin() string {
	if b := os.Getenv("ZOO_CONTAINER_BIN"); b != "" {
		return b
	}
	return "container"
}

// Installed reports whether the CLI is on PATH.
func Installed() bool {
	_, err := exec.LookPath(Bin())
	return err == nil
}

// Run runs the CLI and returns stdout. Errors include stderr.
func Run(args ...string) (string, error) {
	cmd := exec.Command(Bin(), args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("container %s: %v: %s", strings.Join(redact(args), " "), err, msg)
	}
	return out.String(), nil
}

// Attach runs the CLI wired to the terminal.
func Attach(args ...string) error {
	cmd := exec.Command(Bin(), args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// redact hides values of -e KEY=VALUE pairs in error messages.
func redact(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i < len(out)-1; i++ {
		if out[i] == "-e" || out[i] == "--env" {
			if k, _, ok := strings.Cut(out[i+1], "="); ok {
				out[i+1] = k + "=…"
			}
		}
	}
	return out
}

// SystemRunning reports whether the container API service is up.
func SystemRunning() bool {
	_, err := Run("system", "status")
	return err == nil
}

// SystemStart starts the service interactively (it may ask to install a kernel).
func SystemStart() error { return Attach("system", "start") }

func listJSON(args ...string) ([]any, error) {
	out, err := Run(append(args, "--format", "json")...)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("parse %s output: %w", strings.Join(args, " "), err)
	}
	if arr, ok := v.([]any); ok {
		return arr, nil
	}
	return []any{v}, nil
}

// ImageExists reports whether an image reference (name:tag) is present.
func ImageExists(ref string) bool {
	arr, err := listJSON("image", "list")
	if err != nil {
		return false
	}
	name, tag, _ := strings.Cut(ref, ":")
	for _, img := range arr {
		found := false
		jsonx.Walk(img, func(_ string, v any) {
			s, ok := v.(string)
			if !ok {
				return
			}
			if s == ref || strings.HasSuffix(s, "/"+ref) || (tag == "latest" && (s == name || strings.HasSuffix(s, "/"+name))) {
				found = true
			}
		})
		if found {
			return true
		}
	}
	return false
}

// Build builds an image from a context directory.
func Build(tag, contextDir string, buildArgs map[string]string) error {
	args := []string{"build", "-t", tag}
	for k, v := range buildArgs {
		args = append(args, "--build-arg", k+"="+v)
	}
	args = append(args, contextDir)
	return Attach(args...)
}

// NetworkExists reports whether a network with this name exists.
func NetworkExists(name string) bool {
	arr, err := listJSON("network", "list")
	if err != nil {
		return false
	}
	for _, n := range arr {
		if jsonx.String(n, "id", "name") == name {
			return true
		}
		hit := false
		jsonx.Walk(n, func(_ string, v any) {
			if s, ok := v.(string); ok && s == name {
				hit = true
			}
		})
		if hit {
			return true
		}
	}
	return false
}

// CreateInternalNetwork creates a host-only network (macOS 26+).
func CreateInternalNetwork(name string) error {
	_, err := Run("network", "create", "--internal", "--label", "zoo=1", name)
	return err
}

// Container is the part of `container list` zoo cares about.
type Container struct {
	ID     string            `json:"id"`
	Status string            `json:"status"`
	Labels map[string]string `json:"labels"`
	IP     string            `json:"ip"` // address on the zoo network
	Raw    any               `json:"-"`
}

func (c Container) Running() bool {
	s := strings.ToLower(c.Status)
	return s == "running" || strings.HasPrefix(s, "up")
}

// List returns all containers (running or not). network picks which IP to report.
func List(network string) ([]Container, error) {
	arr, err := listJSON("list", "--all")
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(arr))
	for _, el := range arr {
		c := Container{Raw: el, Labels: map[string]string{}}
		c.ID = jsonx.String(el, "id")
		c.Status = jsonx.String(el, "status", "state")
		if m := jsonx.Map(el, "labels"); m != nil {
			for k, v := range m {
				if s, ok := v.(string); ok {
					c.Labels[k] = s
				}
			}
		}
		c.IP = jsonx.IPOnNetwork(el, network)
		out = append(out, c)
	}
	return out, nil
}

// Get finds one container by id.
func Get(id, network string) (Container, bool) {
	all, err := List(network)
	if err != nil {
		return Container{}, false
	}
	for _, c := range all {
		if c.ID == id {
			return c, true
		}
	}
	return Container{}, false
}

// InspectIP returns a container's IPv4 on a network, via `container inspect`.
func InspectIP(id, network string) (string, error) {
	out, err := Run("inspect", id)
	if err != nil {
		return "", err
	}
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", err
	}
	ip := jsonx.IPOnNetwork(v, network)
	if ip == "" {
		return "", fmt.Errorf("no address for %s on network %s", id, network)
	}
	return ip, nil
}

// Stats returns one snapshot of `container stats` for running containers.
func Stats() ([]any, error) { return listJSON("stats", "--no-stream") }

func Stop(id string) error {
	_, err := Run("stop", id)
	return err
}

func Delete(id string) error {
	_, err := Run("delete", "--force", id)
	return err
}

func Start(id string) error {
	_, err := Run("start", id)
	return err
}
