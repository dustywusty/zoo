package main

import (
	"strings"
	"testing"
)

func TestParseGlobal(t *testing.T) {
	flagBackend, flagFloor, flagAllowWeaker = "", "", false
	rest, err := parseGlobal([]string{"up", "--backend", "docker", "--isolation-floor=container", "--allow-weaker-isolation", "--", "--backend", "x"})
	if err != nil {
		t.Fatal(err)
	}
	if flagBackend != "docker" || flagFloor != "container" || !flagAllowWeaker {
		t.Fatalf("%q %q %v", flagBackend, flagFloor, flagAllowWeaker)
	}
	if strings.Join(rest, " ") != "up -- --backend x" {
		t.Fatalf("args after -- belong to the agent: %v", rest)
	}
	if _, err := parseGlobal([]string{"up", "--isolation-floor", "bogus"}); err == nil {
		t.Fatal("bad floor accepted")
	}
	flagBackend, flagFloor, flagAllowWeaker = "", "", false
}
