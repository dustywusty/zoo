package backend

import "testing"

func TestGuestPath(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\me\repo`:          "/c/Users/me/repo",
		`c:\Users\me\repo\`:         "/c/Users/me/repo",
		`D:/src/zoo/.git/worktrees`: "/d/src/zoo/.git/worktrees",
		`C:\`:                       "/c",
		"/Users/me/repo":            "/Users/me/repo",
		"/tmp/repo":                 "/tmp/repo",
		`/odd\name`:                 `/odd\name`, // a legal Unix name, untouched
		"relative/path":             "relative/path",
	} {
		if got := GuestPath(in); got != want {
			t.Errorf("GuestPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckGuestPath(t *testing.T) {
	for _, bad := range []string{`\\server\share\repo`, "//server/share/repo", `C:repo`} {
		if CheckGuestPath(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{`C:\Users\me\repo`, "/Users/me/repo", "/tmp/x"} {
		if err := CheckGuestPath(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}
