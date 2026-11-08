//go:build windows

package keys

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows Credential Manager, through advapi32. Items are generic
// credentials named "zoo:<name>", visible (and removable) in Control Panel
// > Credential Manager > Windows Credentials, readable only by this user.
//
// A credential blob holds at most 2560 bytes, and some items are bigger
// (a Codex ChatGPT login's auth.json carries several tokens), so a value
// is split: "zoo:<name>" holds the first part and says how many there are
// in its comment; "zoo:<name>#1", "#2"... hold the rest.

func platformStore() store { return credman{} }

type credman struct{}

const (
	credTypeGeneric        = 1
	credPersistLocalMach   = 2
	credMaxBlob            = 5 * 512
	errNotFound            = syscall.Errno(1168) // ERROR_NOT_FOUND
	targetPrefix           = "zoo:"
	partSep                = "#"
	commentParts           = "zoo parts="
	credEnumerateAllFilter = 0
)

var (
	advapi32          = windows.NewLazySystemDLL("advapi32.dll")
	procCredWriteW    = advapi32.NewProc("CredWriteW")
	procCredReadW     = advapi32.NewProc("CredReadW")
	procCredDeleteW   = advapi32.NewProc("CredDeleteW")
	procCredEnumerate = advapi32.NewProc("CredEnumerateW")
	procCredFree      = advapi32.NewProc("CredFree")
)

// credential is CREDENTIALW.
type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

func (credman) name() string { return "credential manager" }

func target(name string, part int) string {
	if part == 0 {
		return targetPrefix + name
	}
	return targetPrefix + name + partSep + strconv.Itoa(part)
}

func write(tgt, comment string, blob []byte) error {
	t, err := windows.UTF16PtrFromString(tgt)
	if err != nil {
		return err
	}
	user, _ := windows.UTF16PtrFromString("zoo")
	c := credential{Type: credTypeGeneric, TargetName: t, Persist: credPersistLocalMach, UserName: user,
		CredentialBlobSize: uint32(len(blob))}
	if comment != "" {
		c.Comment, _ = windows.UTF16PtrFromString(comment)
	}
	if len(blob) > 0 {
		c.CredentialBlob = &blob[0]
	}
	if r, _, e := procCredWriteW.Call(uintptr(unsafe.Pointer(&c)), 0); r == 0 {
		return fmt.Errorf("credential manager: write %s: %w", tgt, e)
	}
	return nil
}

// read returns the blob and comment of one credential; ok=false if absent.
func read(tgt string) ([]byte, string, bool, error) {
	t, err := windows.UTF16PtrFromString(tgt)
	if err != nil {
		return nil, "", false, err
	}
	var p *credential
	if r, _, e := procCredReadW.Call(uintptr(unsafe.Pointer(t)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&p))); r == 0 {
		if errors.Is(e, errNotFound) {
			return nil, "", false, nil
		}
		return nil, "", false, fmt.Errorf("credential manager: read %s: %w", tgt, e)
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(p)))
	blob := append([]byte(nil), unsafe.Slice(p.CredentialBlob, p.CredentialBlobSize)...)
	comment := ""
	if p.Comment != nil {
		comment = windows.UTF16PtrToString(p.Comment)
	}
	return blob, comment, true, nil
}

func del(tgt string) error {
	t, err := windows.UTF16PtrFromString(tgt)
	if err != nil {
		return err
	}
	if r, _, e := procCredDeleteW.Call(uintptr(unsafe.Pointer(t)), credTypeGeneric, 0); r == 0 && !errors.Is(e, errNotFound) {
		return fmt.Errorf("credential manager: delete %s: %w", tgt, e)
	}
	return nil
}

func (credman) set(name, value string) error {
	if strings.Contains(name, partSep) {
		return fmt.Errorf("key names can't contain %q", partSep)
	}
	b := []byte(value)
	var parts [][]byte
	for len(b) > credMaxBlob {
		parts, b = append(parts, b[:credMaxBlob]), b[credMaxBlob:]
	}
	parts = append(parts, b)
	// Drop parts left over from a longer old value first.
	if err := (credman{}).remove(name); err != nil {
		return err
	}
	for i := len(parts) - 1; i >= 1; i-- {
		if err := write(target(name, i), "", parts[i]); err != nil {
			return err
		}
	}
	return write(target(name, 0), commentParts+strconv.Itoa(len(parts)), parts[0])
}

func parts(comment string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(comment, commentParts))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func (credman) get(name string) (string, bool, error) {
	first, comment, ok, err := read(target(name, 0))
	if !ok || err != nil {
		return "", false, err
	}
	var b strings.Builder
	b.Write(first)
	for i := 1; i < parts(comment); i++ {
		p, _, ok, err := read(target(name, i))
		if err != nil {
			return "", false, err
		}
		if !ok {
			return "", false, fmt.Errorf("credential manager: %s is missing part %d; store it again", name, i)
		}
		b.Write(p)
	}
	return b.String(), true, nil
}

func (credman) remove(name string) error {
	_, comment, ok, err := read(target(name, 0))
	if err != nil {
		return err
	}
	n := 1
	if ok {
		n = parts(comment)
	}
	for i := n - 1; i >= 0; i-- {
		if err := del(target(name, i)); err != nil {
			return err
		}
	}
	return nil
}

func (credman) list() ([]string, bool) {
	filter, _ := windows.UTF16PtrFromString(targetPrefix + "*")
	var count uint32
	var creds **credential
	if r, _, _ := procCredEnumerate.Call(uintptr(unsafe.Pointer(filter)), credEnumerateAllFilter,
		uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&creds))); r == 0 {
		return nil, true // none (ERROR_NOT_FOUND) or unreadable: nothing to show
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(creds)))
	var out []string
	for _, c := range unsafe.Slice(creds, count) {
		n := strings.TrimPrefix(windows.UTF16PtrToString(c.TargetName), targetPrefix)
		if !strings.Contains(n, partSep) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, true
}
