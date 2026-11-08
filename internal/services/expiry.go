package services

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dusty/zoo/internal/paths"
)

// Grants can be temporary: `zoo grant --for 4h <cage> <service>`. Expiries
// live beside grants.json, keyed "cage/service". An expired grant stops
// counting immediately (LoadGrants drops it), and PruneExpired removes it
// from disk so it doesn't come back.

type grantExpiries map[string]time.Time

func expiryKey(cage, service string) string { return cage + "/" + service }

func loadGrantExpiries() grantExpiries {
	e := grantExpiries{}
	if b, err := os.ReadFile(paths.GrantExpiry()); err == nil {
		_ = json.Unmarshal(b, &e)
	}
	return e
}

func (e grantExpiries) save() error {
	if len(e) == 0 {
		err := os.Remove(paths.GrantExpiry())
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	b, _ := json.MarshalIndent(e, "", "  ")
	tmp := paths.GrantExpiry() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, paths.GrantExpiry())
}

// SetGrantExpiry makes a grant lapse at until; a zero time makes it permanent.
func SetGrantExpiry(cage, service string, until time.Time) error {
	e := loadGrantExpiries()
	if until.IsZero() {
		if _, ok := e[expiryKey(cage, service)]; !ok {
			return nil
		}
		delete(e, expiryKey(cage, service))
	} else {
		e[expiryKey(cage, service)] = until
	}
	return e.save()
}

// GrantExpiry returns when a grant lapses (zero: never).
func GrantExpiry(cage, service string) time.Time {
	return loadGrantExpiries()[expiryKey(cage, service)]
}

// dropExpired removes grants whose time is up from g (in memory).
func (g Grants) dropExpired(now time.Time) [][2]string {
	e := loadGrantExpiries()
	var gone [][2]string
	for k, t := range e {
		if now.Before(t) {
			continue
		}
		cage, svc, ok := strings.Cut(k, "/")
		if ok && g.Remove(cage, svc) {
			gone = append(gone, [2]string{cage, svc})
		}
	}
	sort.Slice(gone, func(i, j int) bool { return gone[i][0]+gone[i][1] < gone[j][0]+gone[j][1] })
	return gone
}

// PruneExpired removes lapsed grants from disk and returns them as
// (cage, service) pairs.
func PruneExpired(now time.Time) ([][2]string, error) {
	g := Grants{}
	if b, err := os.ReadFile(paths.Grants()); err == nil {
		_ = json.Unmarshal(b, &g)
	}
	gone := g.dropExpired(now)
	e := loadGrantExpiries()
	changed := false
	for k, t := range e {
		if !now.Before(t) {
			delete(e, k)
			changed = true
		}
	}
	if len(gone) > 0 {
		if err := g.Save(); err != nil {
			return gone, err
		}
	}
	if changed {
		return gone, e.save()
	}
	return gone, nil
}
