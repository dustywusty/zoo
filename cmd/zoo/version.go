package main

import (
	"runtime/debug"
	"strings"
	"time"
)

// Set at link time by GoReleaser and the Makefile:
//
//	-X main.version=0.1.0 -X main.commit=<sha> -X main.date=<RFC 3339>
//
// A plain `go build` leaves commit and date empty; then the commit and its
// time come from the build info Go embeds from git.
var (
	version = "0.1.0-dev"
	commit  = ""
	date    = ""
)

// versionString is what `zoo version` prints, e.g.
// "zoo 0.1.0 (ca5f248, built 2026-11-08 05:49 UTC)".
func versionString() string {
	return formatVersion(version, commit, date, buildSettings())
}

func buildSettings() map[string]string {
	out := map[string]string{}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			out[s.Key] = s.Value
		}
	}
	return out
}

func formatVersion(ver, rev, built string, vcs map[string]string) string {
	when := "built"
	if rev == "" {
		rev = vcs["vcs.revision"]
		if vcs["vcs.modified"] == "true" && rev != "" {
			rev += "-dirty"
		}
	}
	if built == "" && vcs["vcs.time"] != "" {
		built, when = vcs["vcs.time"], "committed"
	}
	var parts []string
	if rev != "" {
		sha, dirty := strings.CutSuffix(rev, "-dirty")
		if len(sha) > 7 {
			sha = sha[:7]
		}
		if dirty {
			sha += "-dirty"
		}
		parts = append(parts, sha)
	}
	if built != "" {
		if t, err := time.Parse(time.RFC3339, built); err == nil {
			built = t.UTC().Format("2006-01-02 15:04 UTC")
		}
		parts = append(parts, when+" "+built)
	}
	if len(parts) == 0 {
		return "zoo " + ver
	}
	return "zoo " + ver + " (" + strings.Join(parts, ", ") + ")"
}
