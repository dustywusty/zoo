package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dusty/zoo/internal/config"
	"github.com/dusty/zoo/internal/update"
)

func updateServer(t *testing.T, cfg config.Global) *Server {
	t.Helper()
	t.Setenv("ZOO_HOME", t.TempDir())
	s := historyServer()
	s.cfg, s.token, s.svc, s.version = cfg, "tok", newSvcState(), "0.1.1"
	return s
}

func post(t *testing.T, s *Server, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7777/api/update", nil)
	if token != "" {
		req.Header.Set("X-Zoo-Token", token)
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	return w
}

// The update button changes zoo itself, so only the dashboard page (which
// holds the token) can press it, and only when there is an update that zoo
// can install itself.
func TestUpdateButtonIsGuarded(t *testing.T) {
	s := updateServer(t, config.Defaults())
	if w := post(t, s, ""); w.Code != http.StatusForbidden {
		t.Errorf("without the token: %d, want 403", w.Code)
	}
	if w := post(t, s, "tok"); w.Code != http.StatusConflict {
		t.Errorf("with no update known: %d, want 409", w.Code)
	}
	s.update = &update.Status{Current: "0.1.1", Latest: "0.2.0", Available: true, Kind: update.Homebrew, Command: update.Command(update.Homebrew)}
	w := post(t, s, "tok")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "brew upgrade zoo") {
		t.Errorf("Homebrew install: %d %q, want 409 with the brew command", w.Code, w.Body.String())
	}
	s.update = &update.Status{Current: "0.1.1", Latest: "0.2.0", Available: true, Kind: update.Self}
	s.updating = true
	if w := post(t, s, "tok"); w.Code != http.StatusConflict {
		t.Errorf("while an update runs: %d, want 409", w.Code)
	}
}

func fakeLatest(t *testing.T, hits *atomic.Int32) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"tag_name":"v0.2.0","html_url":"https://example.invalid/r"}`))
	}))
	t.Cleanup(srv.Close)
	old := update.APIBase
	update.APIBase = srv.URL
	t.Cleanup(func() { update.APIBase = old })
}

func TestUpdateCheckRespectsTheSetting(t *testing.T) {
	var hits atomic.Int32
	fakeLatest(t, &hits)
	off := false
	cfg := config.Defaults()
	cfg.Updates.Check = &off
	s := updateServer(t, cfg)
	s.checkUpdates(context.Background())
	if hits.Load() != 0 || s.update != nil {
		t.Errorf("updates.check false: %d requests, update %+v; want none", hits.Load(), s.update)
	}
}

func TestUpdateCheckOncePerDay(t *testing.T) {
	var hits atomic.Int32
	fakeLatest(t, &hits)
	s := updateServer(t, config.Defaults())
	s.checkUpdates(context.Background())
	if hits.Load() != 1 || s.update == nil || !s.update.Available || s.update.Latest != "0.2.0" {
		t.Fatalf("first check: %d requests, update %+v", hits.Load(), s.update)
	}
	s.checkUpdates(context.Background())
	if hits.Load() != 1 {
		t.Errorf("second check within a day asked GitHub again (%d requests)", hits.Load())
	}
	st, _ := update.Cached()
	st.CheckedAt = time.Now().Add(-25 * time.Hour)
	_ = update.Save(st)
	s.checkUpdates(context.Background())
	if hits.Load() != 2 {
		t.Errorf("a day later: %d requests, want 2", hits.Load())
	}
}
