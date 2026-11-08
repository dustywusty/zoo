package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTrust(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	root := t.TempDir()
	write := func(s string) {
		if err := os.WriteFile(filepath.Join(root, "zoo.yaml"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// No file, nothing approved: unchanged, empty project.
	if st, _ := CheckTrust(root); st.Changed {
		t.Fatal("no zoo.yaml should be unchanged")
	}

	// A new file is a proposal: not used until approved.
	write("size: medium\nservices: [pg]\n")
	if st, _ := CheckTrust(root); !st.Changed || st.EverTrust {
		t.Fatalf("new zoo.yaml should be pending: %+v", st)
	}
	if p, _ := LoadProject(root); p.Size != "" || len(p.Services) != 0 {
		t.Fatalf("unapproved zoo.yaml was used: %+v", p)
	}
	if _, err := Trust(root); err != nil {
		t.Fatal(err)
	}
	if p, _ := LoadProject(root); p.Size != "medium" || len(p.Services) != 1 {
		t.Fatalf("approved zoo.yaml not used: %+v", p)
	}

	// The agent edits it: the approved copy still applies.
	write("size: medium\nservices: [pg, prod-db]\nunmask: [.env]\n")
	if st, _ := CheckTrust(root); !st.Changed || !st.EverTrust {
		t.Fatalf("edit should be pending: %+v", st)
	}
	if p, _ := LoadProject(root); len(p.Services) != 1 || len(p.Unmask) != 0 {
		t.Fatalf("agent's edit took effect: %+v", p)
	}

	// Deleting it is a change too (it would drop extra masks).
	_ = os.Remove(filepath.Join(root, "zoo.yaml"))
	if st, _ := CheckTrust(root); !st.Changed || st.Present {
		t.Fatalf("deletion should be pending: %+v", st)
	}
	if p, _ := LoadProject(root); p.Size != "medium" {
		t.Fatal("deletion took effect without approval")
	}

	// A file that doesn't parse can't be approved.
	write("size: [unclosed\n")
	if _, err := Trust(root); err == nil {
		t.Fatal("approved an unparseable zoo.yaml")
	}

	// Another worktree's approval doesn't carry over.
	other := t.TempDir()
	if p, _ := LoadProject(other); p.Size != "" {
		t.Fatal("approval leaked across worktrees")
	}
}
