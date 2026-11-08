package main

import "testing"

func TestFormatVersion(t *testing.T) {
	for _, c := range []struct {
		ver, rev, date string
		vcs            map[string]string
		want           string
	}{
		{"0.1.0", "ca5f24866a05f7e0", "2026-11-08T05:49:12Z", nil, "zoo 0.1.0 (ca5f248, built 2026-11-08 05:49 UTC)"},
		{"0.1.0-3-g1b2c3d4-dirty", "1b2c3d4", "2026-11-08T06:00:00-06:00", nil, "zoo 0.1.0-3-g1b2c3d4-dirty (1b2c3d4, built 2026-11-08 12:00 UTC)"},
		{"0.1.0-dev", "", "", map[string]string{"vcs.revision": "deadbeefcafe", "vcs.time": "2026-11-07T10:00:00Z", "vcs.modified": "true"},
			"zoo 0.1.0-dev (deadbee-dirty, committed 2026-11-07 10:00 UTC)"},
		{"0.1.0-dev", "", "", nil, "zoo 0.1.0-dev"},
	} {
		if got := formatVersion(c.ver, c.rev, c.date, c.vcs); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
}
