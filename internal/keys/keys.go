// Package keys stores harness credentials in the macOS Keychain (service "zoo").
// On other systems it falls back to 0600 files under ~/.zoo/keys.
package keys

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/dusty/zoo/internal/paths"
)

const service = "zoo"

func useKeychain() bool {
	if os.Getenv("ZOO_KEYS_FILE") == "1" {
		return false
	}
	_, err := exec.LookPath("security")
	return runtime.GOOS == "darwin" && err == nil
}

func fileDir() string { return filepath.Join(paths.Home(), "keys") }

// Set stores value under name. Empty value on macOS prompts securely via `security`.
func Set(name, value string) error {
	if useKeychain() {
		if value == "" {
			// -w as the last option makes `security` prompt without echo.
			cmd := exec.Command("security", "add-generic-password", "-U", "-s", service, "-a", name, "-w")
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			return cmd.Run()
		}
		cmd := exec.Command("security", "add-generic-password", "-U", "-s", service, "-a", name, "-w", value)
		return cmd.Run()
	}
	if err := os.MkdirAll(fileDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(fileDir(), name), []byte(value), 0o600)
}

// Get returns the stored value, or "" if none.
func Get(name string) (string, error) {
	if useKeychain() {
		var out, errb bytes.Buffer
		cmd := exec.Command("security", "find-generic-password", "-s", service, "-a", name, "-w")
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			if strings.Contains(errb.String(), "could not be found") {
				return "", nil
			}
			return "", errors.New(strings.TrimSpace(errb.String()))
		}
		return strings.TrimRight(out.String(), "\n"), nil
	}
	b, err := os.ReadFile(filepath.Join(fileDir(), name))
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(b), err
}

// Remove deletes a stored value.
func Remove(name string) error {
	if useKeychain() {
		return exec.Command("security", "delete-generic-password", "-s", service, "-a", name).Run()
	}
	return os.Remove(filepath.Join(fileDir(), name))
}

// Has reports whether name is stored, without reading it on macOS.
func Has(name string) bool {
	if useKeychain() {
		return exec.Command("security", "find-generic-password", "-s", service, "-a", name).Run() == nil
	}
	_, err := os.Stat(filepath.Join(fileDir(), name))
	return err == nil
}

// FileNames lists stored names in file mode (the Keychain can't list cheaply).
func FileNames() []string {
	ents, _ := os.ReadDir(fileDir())
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}
