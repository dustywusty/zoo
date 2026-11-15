package daemon

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/dusty/zoo/internal/paths"
	"github.com/dusty/zoo/internal/proc"
	"github.com/dusty/zoo/internal/update"
)

// SetVersion tells the daemon which zoo it runs, for the update banner.
func (s *Server) SetVersion(v string) {
	s.mu.Lock()
	s.version = v
	s.mu.Unlock()
}

// checkUpdates asks GitHub for the latest release at most once a day, and
// only when updates.check is on. A saved check for this version that is
// less than a day old is used instead.
func (s *Server) checkUpdates(ctx context.Context) {
	if !s.cfg.Updates.CheckEnabled() {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	s.mu.Lock()
	cur := s.version
	s.mu.Unlock()
	if cur == "" {
		return
	}
	if st, ok := update.Cached(); ok && st.Current == cur && st.Error == "" && time.Since(st.CheckedAt) < 24*time.Hour {
		s.setUpdate(st)
		return
	}
	st := update.Check(ctx, &http.Client{Timeout: 30 * time.Second}, cur, exe)
	if st.Error != "" {
		log.Printf("update check: %s", st.Error)
	}
	_ = update.Save(st)
	s.setUpdate(st)
}

func (s *Server) setUpdate(st update.Status) {
	s.mu.Lock()
	s.update = &st
	s.mu.Unlock()
	s.broadcastState()
}

// handleUpdate starts `zoo update` for the dashboard's update button. The
// update replaces zoo, refreshes the shims, and restarts this daemon with
// the new zoo, so it runs detached from the daemon. Its output goes to the
// daemon log.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	st := s.update
	switch {
	case s.updating:
		s.mu.Unlock()
		http.Error(w, "an update is already running", http.StatusConflict)
		return
	case st == nil || !st.Available:
		s.mu.Unlock()
		http.Error(w, "no update available", http.StatusConflict)
		return
	case st.Kind != update.Self:
		s.mu.Unlock()
		http.Error(w, "zoo was installed with "+string(st.Kind)+"; to update, run: "+st.Command, http.StatusConflict)
		return
	}
	s.updating = true
	s.mu.Unlock()

	fail := func(err error) {
		s.mu.Lock()
		s.updating = false
		s.mu.Unlock()
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	exe, err := os.Executable()
	if err != nil {
		fail(err)
		return
	}
	logf, err := os.OpenFile(paths.DaemonLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fail(err)
		return
	}
	defer logf.Close()
	cmd := exec.Command(exe, "update")
	cmd.Stdout, cmd.Stderr = logf, logf
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		fail(err)
		return
	}
	_ = cmd.Process.Release()
	s.broadcastState()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"started": true, "to": st.Latest})
}
