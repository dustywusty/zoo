package main

import (
	"strings"
	"testing"

	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/inject"
	"github.com/dusty/zoo/internal/keys"
	"github.com/dusty/zoo/internal/paths"
)

func TestDefaultCredentialsStayOnHost(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	t.Setenv("ZOO_KEYS_FILE", "1")
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"claude", "codex", "codex-auth"} {
		if err := keys.Set(key, "test-only-private-"+key); err != nil {
			t.Fatal(err)
		}
	}
	g := config.Defaults()
	for _, harness := range []string{"claude", "codex"} {
		env, files, missing, err := harnessCredentials(g, g.Harnesses[harness], inject.Active(g, nil), "task-123", nil)
		if err != nil || len(missing) > 0 {
			t.Fatalf("%s: %v %v", harness, missing, err)
		}
		out := strings.Join(env, "\n")
		for _, v := range files {
			out += v
		}
		if strings.Contains(out, "test-only-private") || !strings.Contains(out, "zoocred."+harness+".task-123.") {
			t.Fatalf("%s: raw credential or missing placeholder", harness)
		}
		if harness == "codex" && !strings.Contains(files[".codex/auth.json"], "zoocred.codex.") {
			t.Fatal("Codex auth file did not use the placeholder")
		}
	}
	if err := keys.Remove("codex"); err != nil {
		t.Fatal(err)
	}
	env, files, missing, err := harnessCredentials(g, g.Harnesses["codex"], inject.Active(g, nil), "task-123", nil)
	if err != nil || len(missing) != 1 || files[".codex/auth.json"] != "test-only-private-codex-auth" || strings.Contains(strings.Join(env, "\n"), "OPENAI_API_KEY=") {
		t.Fatalf("subscription fallback broken: missing=%v err=%v", missing, err)
	}
	if _, _, _, err := harnessCredentials(g, g.Harnesses["claude"], nil, "task-123", nil); err == nil {
		t.Fatal("missing broker silently accepted")
	}
}
