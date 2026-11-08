// Package keys stores harness credentials in the OS credential store: the
// macOS Keychain (service "zoo"), Windows Credential Manager (generic
// credentials named "zoo:<name>"), or elsewhere 0600 files under
// ~/.zoo/keys. ZOO_KEYS_FILE=1 forces files everywhere (CI, tests).
package keys

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/term"

	"github.com/dusty/zoo/internal/paths"
	"github.com/dusty/zoo/internal/proc"
)

const service = "zoo"

// store is one place secrets can live.
type store interface {
	name() string
	set(name, value string) error
	get(name string) (string, bool, error) // ok=false: not stored
	remove(name string) error
	list() ([]string, bool) // ok=false: can't list cheaply
}

func current() store {
	if os.Getenv("ZOO_KEYS_FILE") == "1" {
		return fileStore{}
	}
	if s := platformStore(); s != nil {
		return s
	}
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("security"); err == nil {
			return keychain{}
		}
	}
	return fileStore{}
}

// StoreName says where local items live: "keychain", "credential manager"
// or "file".
func StoreName() string { return current().name() }

// Set stores value under name. An empty value prompts on the terminal,
// with input hidden.
func Set(name, value string) error {
	if value == "" {
		if k, ok := current().(keychain); ok {
			return k.promptSet(name)
		}
		v, err := prompt()
		if err != nil {
			return err
		}
		value = v
	}
	if value == "" {
		return errors.New("empty value: nothing stored")
	}
	return current().set(name, value)
}

// prompt reads one line from the terminal without echoing it.
func prompt() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("no terminal to prompt on: pipe the value on stdin")
	}
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// Get returns the stored value, or "" if none.
func Get(name string) (string, error) {
	v, _, err := current().get(name)
	return v, err
}

// Remove deletes a stored value.
func Remove(name string) error { return current().remove(name) }

// Has reports whether name is stored.
func Has(name string) bool {
	_, ok, err := current().get(name)
	return ok && err == nil
}

// Names lists stored names where the store can say (files, Credential
// Manager); nil for the Keychain, which can't list cheaply.
func Names() []string {
	n, _ := current().list()
	return n
}

// FileNames is Names, kept for callers from before stores.
func FileNames() []string { return Names() }

// --- 0600 files ---------------------------------------------------------------

type fileStore struct{}

func fileDir() string { return filepath.Join(paths.Home(), "keys") }

func (fileStore) name() string { return "file" }

func (fileStore) set(name, value string) error {
	if err := os.MkdirAll(fileDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(fileDir(), name), []byte(value), 0o600)
}

func (fileStore) get(name string) (string, bool, error) {
	b, err := os.ReadFile(filepath.Join(fileDir(), name))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	return string(b), err == nil, err
}

func (fileStore) remove(name string) error { return os.Remove(filepath.Join(fileDir(), name)) }

func (fileStore) list() ([]string, bool) {
	ents, _ := os.ReadDir(fileDir())
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, true
}

// --- macOS Keychain, through `security` --------------------------------------

type keychain struct{}

func (keychain) name() string { return "keychain" }

func security(args ...string) (string, string, error) {
	var out, errb bytes.Buffer
	cmd := exec.Command("security", args...)
	proc.Quiet(cmd)
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func (keychain) set(name, value string) error {
	_, e, err := security("add-generic-password", "-U", "-s", service, "-a", name, "-w", value)
	if err != nil {
		return errors.New(strings.TrimSpace(e))
	}
	return nil
}

func (keychain) get(name string) (string, bool, error) {
	out, e, err := security("find-generic-password", "-s", service, "-a", name, "-w")
	if err != nil {
		if strings.Contains(e, "could not be found") {
			return "", false, nil
		}
		return "", false, errors.New(strings.TrimSpace(e))
	}
	return strings.TrimRight(out, "\n"), true, nil
}

func (keychain) remove(name string) error {
	_, e, err := security("delete-generic-password", "-s", service, "-a", name)
	if err != nil {
		return errors.New(strings.TrimSpace(e))
	}
	return nil
}

func (keychain) list() ([]string, bool) { return nil, false }

// promptSet lets `security` prompt, so the secret never appears in a
// command line other local processes could read. (-w as the last option
// makes it prompt without echo.)
func (keychain) promptSet(name string) error {
	cmd := exec.Command("security", "add-generic-password", "-U", "-s", service, "-a", name, "-w")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
