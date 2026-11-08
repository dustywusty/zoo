package services

import (
	"testing"
	"time"
)

func TestGrantExpiry(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	g := LoadGrants()
	g.Add("api-1", "pg")
	g.Add("api-1", "redis")
	if err := g.Save(); err != nil {
		t.Fatal(err)
	}
	if err := SetGrantExpiry("api-1", "pg", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := SetGrantExpiry("api-1", "redis", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Expired grants stop counting at once, before any prune.
	if got := LoadGrants()["api-1"]; len(got) != 1 || got[0] != "redis" {
		t.Fatalf("grants = %v", got)
	}
	gone, err := PruneExpired(time.Now())
	if err != nil || len(gone) != 1 || gone[0] != [2]string{"api-1", "pg"} {
		t.Fatalf("prune = %v, %v", gone, err)
	}
	if !GrantExpiry("api-1", "pg").IsZero() || GrantExpiry("api-1", "redis").IsZero() {
		t.Fatal("expiry records wrong after prune")
	}
	// Re-granting for good clears the expiry.
	_ = SetGrantExpiry("api-1", "redis", time.Time{})
	if _, err := PruneExpired(time.Now().Add(2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := LoadGrants()["api-1"]; len(got) != 1 {
		t.Fatalf("permanent grant lost: %v", got)
	}
}
