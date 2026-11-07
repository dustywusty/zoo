package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseFor reads a `--for` duration: Go syntax ("90m", "4h") plus days ("2d").
func ParseFor(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(d, 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("bad duration %q (e.g. 30m, 4h, 2d)", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("bad duration %q (e.g. 30m, 4h, 2d)", s)
	}
	return d, nil
}

// Until formats an expiry for humans: "3h12m (until Jan 2 14:05)".
func Until(t time.Time) string {
	d := time.Until(t).Round(time.Minute)
	if d <= 0 {
		return "expired"
	}
	return fmt.Sprintf("%s (until %s)", strings.TrimSuffix(d.String(), "0s"), t.Local().Format("Jan 2 15:04"))
}
