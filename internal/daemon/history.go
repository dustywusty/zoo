package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/dusty/zoo/internal/paths"
)

// logCursor includes a small suffix to detect truncation/replacement without
// relying on platform-specific inode numbers. It always ends at a newline.
type logCursor struct {
	Offset int64  `json:"offset"`
	Tail   string `json:"tail"`
}

type history struct {
	Version   int                        `json:"version"`
	Cursor    logCursor                  `json:"cursor"`
	NextID    int64                      `json:"next_id"`
	Events    []NetEvent                 `json:"events"`
	Alerts    []Alert                    `json:"alerts"`
	Counts    map[string]*Counts         `json:"counts"`
	Seen      map[string]map[string]bool `json:"seen"`
	SentBytes map[string]int64           `json:"sent_bytes"`
	Alerted   map[string]time.Time       `json:"alerted"`
	Sessions  map[string]Session         `json:"sessions"`
}

func historyPath() string { return filepath.Join(paths.Home(), "history.json") }

func (s *Server) saveHistory() error {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	s.mu.Lock()
	b, err := json.Marshal(history{Version: 1, Cursor: s.cursor, NextID: s.nextID,
		Events: s.events, Alerts: s.alerts, Counts: s.counts, Seen: s.seen,
		SentBytes: s.sentBytes, Alerted: s.alerted, Sessions: s.sessions})
	s.mu.Unlock()
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(paths.Home(), ".history-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), historyPath())
}

func (s *Server) loadHistory() (bool, error) {
	f, err := os.Open(historyPath())
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	var h history
	if err := json.NewDecoder(io.LimitReader(f, 64<<20)).Decode(&h); err != nil {
		return false, fmt.Errorf("read %s: %w", historyPath(), err)
	}
	if h.Version != 1 || h.Cursor.Offset < 0 || len(h.Cursor.Tail) > 128 || int64(len(h.Cursor.Tail)) > h.Cursor.Offset {
		return false, fmt.Errorf("invalid history in %s", historyPath())
	}
	if len(h.Events) > maxEvents {
		h.Events = h.Events[len(h.Events)-maxEvents:]
	}
	if len(h.Alerts) > 500 {
		h.Alerts = h.Alerts[len(h.Alerts)-500:]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events, s.alerts, s.nextID, s.cursor = h.Events, h.Alerts, h.NextID, h.Cursor
	if h.Counts != nil {
		s.counts = h.Counts
	}
	if h.Seen != nil {
		s.seen = h.Seen
	}
	if h.SentBytes != nil {
		s.sentBytes = h.SentBytes
	}
	if h.Alerted != nil {
		s.alerted = h.Alerted
	}
	if h.Sessions != nil {
		s.sessions = h.Sessions
	}
	return true, nil
}

func cursorAt(offset int64, line []byte) logCursor {
	if len(line) > 128 {
		line = line[len(line)-128:]
	}
	return logCursor{Offset: offset, Tail: string(line)}
}

func cursorMatches(c logCursor) bool {
	if c.Offset == 0 {
		return true
	}
	f, err := os.Open(paths.AccessLog())
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, len(c.Tail))
	_, err = f.ReadAt(buf, c.Offset-int64(len(buf)))
	return err == nil && len(buf) > 0 && string(buf) == c.Tail
}

// initialCursor leaves a partial final line for the live tailer.
func initialCursor() logCursor {
	f, err := os.Open(paths.AccessLog())
	if err != nil {
		return logCursor{}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return logCursor{}
	}
	n := min(st.Size(), 1<<20)
	b := make([]byte, n)
	if _, err := f.ReadAt(b, st.Size()-n); err != nil {
		return logCursor{}
	}
	i := bytes.LastIndexByte(b, '\n')
	if i < 0 {
		return logCursor{}
	}
	return cursorAt(st.Size()-n+int64(i+1), b[:i+1])
}
