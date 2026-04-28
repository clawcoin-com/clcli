package daemon

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// disengageSet tracks post IDs that this daemon has decided to stop
// engaging with — typically because the post has negative community karma
// and the brain has chosen to skip. Each entry has a TTL (default 24h)
// so a temporarily-bad post can re-enter the engagement pool later.
//
// Persistence: the set is mirrored as a JSON-lines file on disk (one entry
// per line). On startup the daemon loads the file, drops expired entries,
// and rewrites a clean copy. Subsequent additions append a single line.
//
// The file is written under the daemon's profile directory by default
// (`<profile>/disengage.jsonl`). If a daemon crashes mid-write, the worst
// case is one corrupted trailing line, which load() skips silently.
type disengageSet struct {
	mu   sync.Mutex
	ids  map[string]time.Time
	ttl  time.Duration
	path string // empty => in-memory only
}

type disengageEntry struct {
	PostID  string    `json:"post_id"`
	AddedAt time.Time `json:"added_at"`
}

// newDisengageSet returns a set with the given TTL. If path is non-empty,
// it loads existing entries from disk (expired ones discarded) and arms
// future Add() calls to append to that file.
func newDisengageSet(path string, ttl time.Duration) *disengageSet {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	s := &disengageSet{
		ids:  make(map[string]time.Time),
		ttl:  ttl,
		path: path,
	}
	if path != "" {
		s.load()
	}
	return s
}

// Contains returns true if postID is currently in the set (and not expired).
func (s *disengageSet) Contains(postID string) bool {
	if postID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.ids[postID]
	if !ok {
		return false
	}
	if time.Since(t) > s.ttl {
		delete(s.ids, postID)
		return false
	}
	return true
}

// Add records postID with the current time. Idempotent: if already present
// the timestamp is refreshed (extends the TTL) but no second disk line is
// written.
func (s *disengageSet) Add(postID string) {
	if postID == "" {
		return
	}
	s.mu.Lock()
	_, existed := s.ids[postID]
	now := time.Now()
	s.ids[postID] = now
	s.mu.Unlock()

	if existed {
		return
	}
	s.append(postID, now)
}

// Size returns the live entry count.
func (s *disengageSet) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-s.ttl)
	live := 0
	for _, t := range s.ids {
		if t.After(cutoff) {
			live++
		}
	}
	return live
}

// load reads s.path and populates the in-memory map, discarding expired
// entries. Then rewrites the file with only surviving entries to keep
// the file from growing forever.
func (s *disengageSet) load() {
	f, err := os.Open(s.path)
	if err != nil {
		return // file does not exist yet — fine
	}
	defer f.Close()

	cutoff := time.Now().Add(-s.ttl)
	live := []disengageEntry{}
	rawLines := 0

	scanner := bufio.NewScanner(f)
	// Allow reasonably large lines (post IDs are short but be safe).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		rawLines++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e disengageEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue // skip malformed line
		}
		if e.AddedAt.Before(cutoff) {
			continue
		}
		live = append(live, e)
	}

	for _, e := range live {
		s.ids[e.PostID] = e.AddedAt
	}

	// If the file had stale entries, compact it.
	if len(live) < rawLines {
		s.rewrite(live)
	}
}

// append writes one entry to the disk file. Best-effort.
func (s *disengageSet) append(postID string, added time.Time) {
	if s.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	line, err := json.Marshal(disengageEntry{PostID: postID, AddedAt: added})
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
}

// rewrite atomically replaces the disk file with the compact live entries.
func (s *disengageSet) rewrite(live []disengageEntry) {
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return
	}
	enc := json.NewEncoder(f)
	for _, e := range live {
		if err := enc.Encode(e); err != nil {
			f.Close()
			_ = os.Remove(tmp)
			return
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return
	}
	_ = os.Rename(tmp, s.path)
}
