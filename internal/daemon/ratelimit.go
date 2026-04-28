package daemon

import (
	"sync"
	"time"
)

// actionLimiter enforces an hourly cap on daemon-initiated actions. It does
// NOT try to substitute for the server-side quota — it layers ON TOP so a
// runaway brain can't empty the server quota in seconds.
//
// The limiter uses a sliding 1-hour window. The daemon calls Try() before
// executing an action: a false result means "cooling down, skip this cycle".
type actionLimiter struct {
	mu       sync.Mutex
	events   []time.Time
	capacity int
	window   time.Duration
}

// newActionLimiter returns a limiter that allows `capacity` events per hour.
// capacity <= 0 disables the limit (Try always returns true).
func newActionLimiter(capacity int) *actionLimiter {
	return &actionLimiter{
		events:   make([]time.Time, 0, capacity),
		capacity: capacity,
		window:   time.Hour,
	}
}

// Try records an action attempt if there is capacity. Returns true if the
// action may proceed, false if the limiter is saturated. Safe to call from
// multiple goroutines (though the daemon is single-threaded by design).
func (l *actionLimiter) Try() bool {
	if l.capacity <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := time.Now().Add(-l.window)
	// Drop expired entries (events array stays sorted by construction).
	drop := 0
	for _, t := range l.events {
		if t.Before(cutoff) {
			drop++
			continue
		}
		break
	}
	if drop > 0 {
		l.events = l.events[drop:]
	}

	if len(l.events) >= l.capacity {
		return false
	}
	l.events = append(l.events, time.Now())
	return true
}

// Remaining returns how many more actions the limiter allows in the current
// window. Useful for status / logging.
func (l *actionLimiter) Remaining() int {
	if l.capacity <= 0 {
		return -1 // unlimited
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := time.Now().Add(-l.window)
	active := 0
	for _, t := range l.events {
		if t.After(cutoff) {
			active++
		}
	}
	if active >= l.capacity {
		return 0
	}
	return l.capacity - active
}

// postReplyLimiter caps how many times this daemon may reply to the SAME post
// in a sliding 1-hour window. The intent is anti-pingpong: even if the brain
// keeps choosing "reply" after every cycle (e.g. because of a runaway
// reply_to_me chain), the daemon will refuse to actually post the reply once
// the per-post cap is reached.
//
// Memory layout: map[postID] -> ring of timestamps (sorted ascending).
// Old entries get garbage-collected lazily on every Try() call.
type postReplyLimiter struct {
	mu       sync.Mutex
	history  map[string][]time.Time
	capacity int
	window   time.Duration
}

// newPostReplyLimiter returns a limiter that allows `capacity` replies per
// post per sliding hour. capacity <= 0 disables the limit.
func newPostReplyLimiter(capacity int) *postReplyLimiter {
	return &postReplyLimiter{
		history:  make(map[string][]time.Time),
		capacity: capacity,
		window:   time.Hour,
	}
}

// Try records a reply attempt to postID if there is capacity for that post.
// Returns true when the reply may proceed.
func (l *postReplyLimiter) Try(postID string) bool {
	if l.capacity <= 0 {
		return true
	}
	if postID == "" {
		// Defensive: missing post_id should not be silently rate-limited.
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := time.Now().Add(-l.window)
	fresh := make([]time.Time, 0, len(l.history[postID]))
	for _, t := range l.history[postID] {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) >= l.capacity {
		l.history[postID] = fresh
		return false
	}
	fresh = append(fresh, time.Now())
	l.history[postID] = fresh
	return true
}

// Forget purges all history for a post. Call after a successful skip-decision
// chain breaks (rare; mostly useful in tests). Not currently called.
func (l *postReplyLimiter) Forget(postID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.history, postID)
}
