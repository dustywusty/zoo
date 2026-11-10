package daemon

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dusty/zoo/internal/cage"
	"github.com/dusty/zoo/internal/keeper"
	"github.com/dusty/zoo/internal/paths"
)

func historyServer() *Server {
	return &Server{counts: map[string]*Counts{}, seen: map[string]map[string]bool{}, sentBytes: map[string]int64{}, alerted: map[string]time.Time{}, sessions: map[string]Session{}, subs: map[chan []byte]struct{}{}}
}

func TestHistorySurvivesRestartWithoutReattribution(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	line := "1761636000.123 192.168.64.5 CONNECT example.com:443 403 TCP_DENIED 3812 214 0\n"
	if err := os.WriteFile(paths.AccessLog(), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	s := historyServer()
	s.cages = []cage.Info{{Name: "original", IP: "192.168.64.5"}}
	e, ok := keeper.ParseLine(line)
	if !ok {
		t.Fatal("invalid fixture")
	}
	c := cursorAt(int64(len(line)), []byte(line))
	s.ingestAt(e, true, &c)
	if err := s.saveHistory(); err != nil {
		t.Fatal(err)
	}
	restored := historyServer()
	loaded, err := restored.loadHistory()
	if err != nil || !loaded {
		t.Fatalf("load: %v %v", loaded, err)
	}
	restored.cages = []cage.Info{{Name: "replacement", IP: "192.168.64.5"}}
	if len(restored.events) != 1 || restored.events[0].Cage != "original" || len(restored.alerts) != 1 || restored.counts["original"].Requests != 1 {
		t.Fatal("history, attribution, or counters lost")
	}
	if restored.nextID != s.nextID || !cursorMatches(restored.cursor) {
		t.Fatal("cursor or IDs lost")
	}
	// Restart from the saved cursor, then process a new request. The old line
	// must not be counted again or assigned to the replacement cage.
	second := "1761636001.123 192.168.64.5 CONNECT example.com:443 403 TCP_DENIED 3812 214 0\n"
	f, err := os.OpenFile(paths.AccessLog(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(second); err != nil {
		t.Fatal(err)
	}
	f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); restored.tail(ctx, restored.cursor.Offset) }()
	defer func() { cancel(); <-done }()
	deadline := time.After(3 * time.Second)
	for {
		restored.mu.Lock()
		n := len(restored.events)
		restored.mu.Unlock()
		if n == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("tail did not resume")
		case <-time.After(10 * time.Millisecond):
		}
	}
	restored.mu.Lock()
	defer restored.mu.Unlock()
	if restored.counts["original"].Requests != 1 || restored.events[1].Cage != "replacement" {
		t.Fatal("replayed or reattributed old traffic")
	}
}

func TestHistoryCursorDetectsRotationAndPartialLines(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.AccessLog(), []byte("complete\npartial"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := initialCursor()
	if c.Offset != int64(len("complete\n")) || !cursorMatches(c) {
		t.Fatalf("partial line consumed: %+v", c)
	}
	if err := os.WriteFile(paths.AccessLog(), []byte("replaced\npartial and more"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cursorMatches(c) {
		t.Fatal("replacement accepted as same log")
	}
}

func TestCorruptHistoryIsReported(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	if err := os.WriteFile(historyPath(), []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := historyServer().loadHistory(); err == nil {
		t.Fatal("corrupt history silently ignored")
	}
}

func TestLiveHistoryDetectsReplacement(t *testing.T) {
	t.Setenv("ZOO_HOME", t.TempDir())
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	first := "1761636000.123 192.168.64.5 CONNECT first.example:443 403 TCP_DENIED 3812 214 0\n"
	if err := os.WriteFile(paths.AccessLog(), []byte(first), 0600); err != nil {
		t.Fatal(err)
	}
	s := historyServer()
	e, _ := keeper.ParseLine(first)
	c := cursorAt(int64(len(first)), []byte(first))
	s.ingestAt(e, true, &c)
	// A larger replacement must be read from its start, even without a shrink.
	replacement := "1761636001.123 192.168.64.5 CONNECT replacement.example:443 403 TCP_DENIED 3812 214 0\n"
	if err := os.WriteFile(paths.AccessLog(), []byte(replacement), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.tail(ctx, c.Offset) }()
	defer func() { cancel(); <-done }()
	deadline := time.After(3 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.events)
		host := s.events[n-1].Host
		s.mu.Unlock()
		if n == 2 && host == "replacement.example" {
			return
		}
		select {
		case <-deadline:
			t.Fatal("replacement log was skipped")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
