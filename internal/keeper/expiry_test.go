package keeper

import (
	"strings"
	"testing"
	"time"
)

func TestAllowForExpires(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	if err := EnsureFiles(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := AllowFor(now.Add(time.Hour), "logs.example.com", "github.com"); err != nil {
		t.Fatal(err)
	}
	list, _ := ReadAllowlist()
	if !Allowed(list, "logs.example.com") {
		t.Fatal("temporary host not allowed")
	}
	// github.com was already allowed for good: --for must not shorten it.
	if _, ok := LoadExpiries()["github.com"]; ok {
		t.Fatal("a permanent entry got an expiry")
	}
	if removed, _ := Expire(now.Add(30 * time.Minute)); len(removed) != 0 {
		t.Fatalf("expired early: %v", removed)
	}
	removed, err := Expire(now.Add(2 * time.Hour))
	if err != nil || strings.Join(removed, ",") != "logs.example.com" {
		t.Fatalf("expire = %v, %v", removed, err)
	}
	list, _ = ReadAllowlist()
	if Allowed(list, "logs.example.com") || !Allowed(list, "github.com") {
		t.Fatalf("after expiry: %v", list)
	}
	if len(LoadExpiries()) != 0 {
		t.Fatal("expiry record left behind")
	}

	// A plain allow makes a temporary entry permanent.
	_, _ = AllowFor(now.Add(time.Hour), "ci.example.com")
	_, _ = Allow("ci.example.com")
	_ = MakePermanent("ci.example.com")
	if removed, _ := Expire(now.Add(2 * time.Hour)); len(removed) != 0 {
		t.Fatalf("permanent entry expired: %v", removed)
	}
	// Deny clears the record.
	_, _ = AllowFor(now.Add(time.Hour), "tmp.example.com")
	_, _ = Deny("tmp.example.com")
	if _, ok := LoadExpiries()["tmp.example.com"]; ok {
		t.Fatal("deny left an expiry")
	}
}
