package keeper

import (
	"encoding/json"
	"os"
	"sort"
	"time"

	"github.com/dusty/zoo/internal/paths"
)

// Temporary allowlist entries: `zoo allow --for 4h host` and the dashboard's
// Allow-for-a-while. The allowlist file stays plain squid input; expiries
// live beside it on the host and the daemon (or the next zoo command)
// removes entries whose time is up.

// Expiries maps an allowlist entry to when it lapses.
type Expiries map[string]time.Time

func LoadExpiries() Expiries {
	e := Expiries{}
	if b, err := os.ReadFile(paths.AllowExpiry()); err == nil {
		_ = json.Unmarshal(b, &e)
	}
	return e
}

func (e Expiries) Save() error {
	if len(e) == 0 {
		err := os.Remove(paths.AllowExpiry())
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	b, _ := json.MarshalIndent(e, "", "  ")
	tmp := paths.AllowExpiry() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, paths.AllowExpiry())
}

// AllowFor allows hosts until a time. A host already allowed for good stays
// allowed for good; one allowed for a while gets the later of the two times.
func AllowFor(until time.Time, hosts ...string) ([]string, error) {
	current, err := ReadAllowlist()
	if err != nil {
		return nil, err
	}
	exp := LoadExpiries()
	permanent := map[string]bool{}
	for _, h := range current {
		if _, temp := exp[h]; !temp {
			permanent[h] = true
		}
	}
	added, err := Allow(hosts...)
	if err != nil {
		return nil, err
	}
	var touched []string
	for _, h := range hosts {
		h = NormalizeHost(h)
		if h == "" || permanent[h] {
			continue
		}
		if covered(current, h) && !inList(added, h) && exp[h].IsZero() {
			continue // covered by a permanent wildcard
		}
		if until.After(exp[h]) {
			exp[h] = until
		}
		touched = append(touched, h)
	}
	if err := exp.Save(); err != nil {
		return added, err
	}
	return touched, nil
}

// MakePermanent drops any expiry on hosts (a plain `zoo allow`).
func MakePermanent(hosts ...string) error {
	exp := LoadExpiries()
	changed := false
	for _, h := range hosts {
		h = NormalizeHost(h)
		if _, ok := exp[h]; ok {
			delete(exp, h)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return exp.Save()
}

// Expire removes allowlist entries whose time is up. Returns what it removed;
// the caller reloads squid when that's non-empty.
func Expire(now time.Time) ([]string, error) {
	exp := LoadExpiries()
	var due []string
	for h, t := range exp {
		if !now.Before(t) {
			due = append(due, h)
		}
	}
	if len(due) == 0 {
		return nil, nil
	}
	sort.Strings(due)
	removed, err := Deny(due...)
	if err != nil {
		return nil, err
	}
	for _, h := range due {
		delete(exp, h)
	}
	return removed, exp.Save()
}

func covered(list []string, h string) bool {
	for _, e := range list {
		if e != h && Covers(e, h) {
			return true
		}
	}
	return false
}

func inList(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
