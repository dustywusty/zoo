//go:build windows

package keys

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Round-trips through the real Credential Manager under a throwaway name,
// removed afterwards.
func TestCredentialManager(t *testing.T) {
	s := credman{}
	name := fmt.Sprintf("zoo-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = s.remove(name) })

	if _, ok, err := s.get(name); ok || err != nil {
		t.Fatalf("absent item: ok=%v err=%v", ok, err)
	}
	big := strings.Repeat("0123456789abcdef", 450) // 7200 bytes: 3 parts
	if err := s.set(name, big); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := s.get(name); !ok || err != nil || v != big {
		t.Fatalf("big value: ok=%v err=%v len=%d", ok, err, len(v))
	}
	names, _ := s.list()
	found := false
	for _, n := range names {
		if n == name {
			found = true
		}
		if strings.Contains(n, partSep) {
			t.Errorf("list shows a part: %s", n)
		}
	}
	if !found {
		t.Errorf("%s not listed in %v", name, names)
	}
	// Shrinking drops the extra parts.
	if err := s.set(name, "small"); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := s.get(name); v != "small" {
		t.Fatalf("after shrink: %q", v)
	}
	if _, _, ok, _ := read(target(name, 1)); ok {
		t.Error("stale part left behind")
	}
	if err := s.remove(name); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.get(name); ok {
		t.Error("still there after remove")
	}
}
