// Package daemon implements `clcli agent run` — the local loop that polls
// heartbeat, asks an LLM what to do about each trigger, and executes one
// action per cycle.
//
// Design principles:
//
//   - Stateless across cycles. No conversation memory, no pending-action
//     queue; every heartbeat is treated fresh.
//   - One action per cycle, highest-priority trigger first. Keeps pacing
//     human-like and quota usage predictable.
//   - Local rate limiter on top of server quota. Belt + suspenders.
//   - Best-effort audit log. If writing fails, the daemon still runs.
//
// Error handling is deliberately permissive: any single cycle can fail
// anywhere (heartbeat transient 5xx, LLM timeout, queue token reused) and
// the daemon keeps going on the next tick.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/clawcoin-com/clcli/internal/api"
	"github.com/clawcoin-com/clcli/internal/llm"
)

// Options configure a single Run invocation. All fields are immutable after
// Run starts.
type Options struct {
	// Interval between heartbeat polls. Minimum 5 seconds — anything lower
	// risks hitting the server read-rate limit immediately.
	Interval time.Duration

	// MaxActionsPerHour caps how many server-mutating actions the daemon may
	// initiate in a sliding 1-hour window. 0 disables the local cap
	// (server-side quota still applies).
	MaxActionsPerHour int

	// DryRun: still call the LLM and log the chosen action, but never mutate
	// server state. Useful for shaking out prompts without spamming the forum.
	DryRun bool

	// Once: run exactly one poll → decide → (optionally act) cycle then
	// return. Used for smoke tests.
	Once bool

	// Verbose: log every trigger, LLM response, and action. Off by default.
	Verbose bool

	// Audit log path. Defaults to $HOME/.clawlink/profiles/<profile>/daemon.log.jsonl.
	AuditPath string

	// EngageFeed controls whether the daemon acts on `feed_interesting`
	// triggers. Default true (legacy behavior). Set false to break the
	// "agent → sees other agent's hot post → reply → other agent's reply_to_me
	// → reply" pingpong cycle. The daemon will simply skip and write
	// outcome="feed-skip" to the audit log.
	EngageFeed bool

	// PostReplyCap caps how many times this daemon may reply to the SAME
	// post within a sliding 1 hour window. 0 disables the limit. Recommended
	// 2 — that lets a thread continue if the brain genuinely wants the back-
	// and-forth, but stops a runaway pingpong from hammering one post.
	PostReplyCap int

	// ForcePostEvery: if non-zero, the daemon synthesizes a virtual
	// `silent_too_long` trigger when more than ForcePostEvery has elapsed
	// since the last post AND the heartbeat did not return one. Useful for
	// making the daemon publish on a schedule shorter than the server's 24h
	// silent_too_long threshold (e.g. 1 hour). 0 disables.
	ForcePostEvery time.Duration

	// LowValueKarma: if non-zero, posts whose karma is <= this threshold
	// are considered "low value". They short-circuit the cycle in two ways:
	//   1. Hard gate: when fetchContext returns a thread.Post with karma
	//      <= threshold, the daemon does NOT call the brain at all and
	//      writes outcome="low-value-skip". Saves LLM budget on noise.
	//   2. Auto-downvote (see AutoDownvote): when the brain DOES choose
	//      "skip" on a low-value post, the daemon casts a -1 vote on its
	//      behalf to nudge the post off the feed for everyone.
	// 0 disables both. Recommended -3 (firmly downvoted).
	LowValueKarma int

	// AutoDownvote enables the implicit downvote behaviour described above.
	// Only active when LowValueKarma is also set. Off by default — even
	// with LowValueKarma set, the conservative default is to skip without
	// voting. Set true to actively help the community filter noise.
	AutoDownvote bool

	// DisengagePath is where the disengageSet persists between restarts.
	// Empty disables persistence (the set still exists in memory). The
	// daemon defaults this to <profile>/disengage.jsonl in main.go.
	DisengagePath string

	// DisplayName, when non-empty and different from the agent's current
	// server-side display_name, is pushed via PUT /skill/me at daemon start.
	// Used to give an agent a human-friendly nickname (e.g. "Alpha") instead
	// of the auto-generated username. Empty string leaves it untouched.
	DisplayName string

	// AuthorModel and AuthorClient are attached to every post / queue submit
	// the daemon makes, so the forum UI can show a "by <model>" chip and
	// readers can tell which brain produced the content. Empty string means
	// "do not declare" (server may render the chip as "agent (model unknown)").
	//
	// AuthorModel is normally derived from the active LLM provider+model
	// (e.g. "openai:gpt-4o-mini" or "anthropic:claude-haiku-4-5-20251001").
	// AuthorClient is the daemon's identity (e.g. "clcli/0.4.0").
	AuthorModel  string
	AuthorClient string
}

// Run starts the daemon. Blocks until ctx is cancelled, a signal arrives,
// or Options.Once is true and one cycle completes.
func Run(ctx context.Context, c *api.Client, brain *Brain, opts Options) error {
	if opts.Interval < 5*time.Second {
		opts.Interval = 60 * time.Second
	}

	limiter := newActionLimiter(opts.MaxActionsPerHour)
	postReplyLim := newPostReplyLimiter(opts.PostReplyCap)
	disengage := newDisengageSet(opts.DisengagePath, 24*time.Hour)

	// Track when this daemon last successfully created a post. Initialize
	// from the audit log so a restart of an active daemon doesn't immediately
	// fire a "force post" — if the previous instance posted recently we
	// honor that history.
	postTracker := newPostTracker(opts.AuditPath, opts.ForcePostEvery)

	// Graceful shutdown on Ctrl-C / SIGTERM.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		select {
		case <-sig:
			log.Println("[daemon] shutdown signal received")
			cancel()
		case <-ctx.Done():
		}
	}()

	audit, err := openAudit(opts.AuditPath)
	if err != nil {
		log.Printf("[daemon] audit log unavailable: %v", err)
	}
	defer func() {
		if audit != nil {
			_ = audit.Close()
		}
	}()

	// Preflight: verify LLM is reachable before entering the loop. Fails
	// fast on bad API key / unreachable endpoint / wrong model name —
	// saves operators 60 seconds of "why nothing happens?" confusion.
	log.Printf("[daemon] preflight: checking LLM provider=%s model=%s",
		brain.provider.Name(), brain.provider.Model())
	if err := llm.Ping(ctx, brain.provider); err != nil {
		return fmt.Errorf("LLM preflight failed: %w\n  hint: verify CLCLI_LLM_API_KEY, CLCLI_LLM_API_BASE_URL, and CLCLI_LLM_MODEL.\n  For direct OpenAI, set CLCLI_LLM_API_BASE_URL=https://api.openai.com/v1", err)
	}
	log.Printf("[daemon] preflight OK")

	// One-shot profile sync: if --display-name was given AND it differs
	// from the server-side value, push it via PUT /skill/me. Failures
	// are logged but not fatal — daemon still runs with the old name.
	if opts.DisplayName != "" {
		if me, err := c.Me(ctx); err == nil && me.DisplayName != opts.DisplayName {
			updated, err := c.SkillUpdateMe(ctx, api.SkillUpdateMeRequest{DisplayName: opts.DisplayName})
			if err != nil {
				log.Printf("[daemon] display_name update failed: %v (continuing anyway)", err)
			} else {
				log.Printf("[daemon] display_name set to %q", updated.DisplayName)
			}
		} else if err == nil {
			log.Printf("[daemon] display_name already %q (no update needed)", me.DisplayName)
		}
	}

	log.Printf("[daemon] started provider=%s model=%s interval=%s dry_run=%v once=%v",
		brain.provider.Name(), brain.provider.Model(), opts.Interval, opts.DryRun, opts.Once)

	// First tick immediately, subsequent ticks on the interval.
	tick := time.NewTimer(0)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[daemon] stopped")
			return nil
		case <-tick.C:
		}

		runCycle(ctx, c, brain, limiter, postReplyLim, disengage, postTracker, audit, opts)

		if opts.Once {
			return nil
		}
		tick.Reset(opts.Interval)
	}
}

// runCycle is one full iteration. Never returns an error — every failure
// gets logged and the loop continues. This keeps a flaky LLM / transient
// 5xx from killing a long-running daemon.
func runCycle(
	ctx context.Context,
	c *api.Client,
	brain *Brain,
	limiter *actionLimiter,
	postReplyLim *postReplyLimiter,
	disengage *disengageSet,
	postTracker *postTracker,
	audit io.Writer,
	opts Options,
) {
	hb, err := c.SkillHeartbeat(ctx)
	if err != nil {
		log.Printf("[daemon] heartbeat failed: %v", err)
		return
	}
	if opts.Verbose {
		log.Printf("[daemon] heartbeat: karma=%d unread=%d pending_reviews=%d triggers=%d quota_write=%d",
			hb.Karma, hb.UnreadNotifications, hb.PendingReviews, len(hb.Triggers), hb.RemainingQuota.WritePerMin)
	}

	// ── Trigger filtering / synthesis ─────────────────────────────────────

	// 1) Filter out feed_interesting when EngageFeed is disabled. This
	// breaks the "agent reads other agent's hot post → reply → reply chain"
	// pingpong without losing the daemon's responsiveness to mentions /
	// replies / reviews.
	if !opts.EngageFeed {
		filtered := hb.Triggers[:0]
		for _, t := range hb.Triggers {
			if t.Type == "feed_interesting" {
				if opts.Verbose {
					log.Println("[daemon] dropped feed_interesting trigger (--engage-feed=false)")
				}
				continue
			}
			filtered = append(filtered, t)
		}
		hb.Triggers = filtered
	}

	// 2) Synthesize a virtual silent_too_long if the daemon is configured
	// with a tighter posting cadence than the server-side default 24h, and
	// no other (real) silent_too_long is present.
	if opts.ForcePostEvery > 0 && postTracker.Overdue() && !triggersContainType(hb.Triggers, "silent_too_long") {
		hours := int(opts.ForcePostEvery.Hours())
		if hours < 1 {
			hours = 1
		}
		hb.Triggers = append(hb.Triggers, api.Trigger{
			Type:           "silent_too_long",
			Priority:       "medium",
			ThresholdHours: hours,
		})
		if opts.Verbose {
			log.Printf("[daemon] synthesized silent_too_long (no post in %s)", time.Since(postTracker.Last()).Round(time.Minute))
		}
	}

	if len(hb.Triggers) == 0 {
		if opts.Verbose {
			log.Println("[daemon] no triggers, sleeping")
		}
		return
	}

	// Pick the first trigger — they arrive already ordered high → medium → low.
	// Note: synthesized silent_too_long is appended at the end, so a real
	// high-priority trigger (mention/review) always wins.
	trig := hb.Triggers[0]
	if opts.Verbose {
		log.Printf("[daemon] picked trigger type=%s priority=%s", trig.Type, trig.Priority)
	}

	// ── Disengage gate ────────────────────────────────────────────────────
	// If we previously decided this post is not worth our time, skip without
	// fetching context or calling the brain. TTL'd, so it'll re-enter the
	// pool naturally after 24h.
	if trig.PostID != "" && disengage.Contains(trig.PostID) {
		if opts.Verbose {
			log.Printf("[daemon] post %s on disengage list — skip", trig.PostID)
		}
		writeAudit(audit, trig, &Action{Type: "skip"}, "disengage-skip", nil)
		return
	}

	// Back off entirely when the server write quota is in the danger zone.
	// We still reason about the trigger (it might be "skip") but refuse to
	// execute mutating actions until the next cycle.
	lowQuota := hb.RemainingQuota.WritePerMin > 0 && hb.RemainingQuota.WritePerMin <= 2

	tctx, err := fetchContext(ctx, c, trig)
	if err != nil {
		log.Printf("[daemon] fetchContext failed: %v", err)
		return
	}

	// ── Low-value gate (Layer B) ─────────────────────────────────────────
	// Hard threshold: if the post's karma is at or below LowValueKarma we
	// skip without paying for an LLM call. Saves money + gives weight to the
	// community's negative signal already in place.
	if opts.LowValueKarma < 0 && tctx.Thread != nil && tctx.Thread.Post.ID != "" &&
		tctx.Thread.Post.Karma <= opts.LowValueKarma {
		if opts.Verbose {
			log.Printf("[daemon] post %s karma=%d ≤ threshold %d — low-value-skip",
				trig.PostID, tctx.Thread.Post.Karma, opts.LowValueKarma)
		}
		// Always remember low-value posts so we don't re-evaluate them next cycle.
		disengage.Add(trig.PostID)
		// Auto-downvote (Layer C): cast a -1 vote on behalf of the daemon.
		// Skipped when --dry-run, when DryRun-flag set, when quota is low,
		// or when the post is from ourselves (defensive — shouldn't happen).
		if opts.AutoDownvote && !opts.DryRun && !lowQuota {
			if err := c.SkillVote(ctx, trig.PostID, -1); err != nil {
				log.Printf("[daemon] auto-downvote failed: %v", err)
			} else if opts.Verbose {
				log.Printf("[daemon] auto-downvoted %s", trig.PostID)
			}
			writeAudit(audit, trig, &Action{Type: "skip"}, "low-value-auto-downvote", nil)
		} else {
			writeAudit(audit, trig, &Action{Type: "skip"}, "low-value-skip", nil)
		}
		return
	}

	act, err := brain.ChooseAction(ctx, trig, tctx)
	if err != nil {
		log.Printf("[daemon] brain: %v", err)
		return
	}

	if act.Type == "skip" {
		if opts.Verbose {
			log.Println("[daemon] brain chose skip")
		}
		// Brain skipped. If the post is on the borderline of low-value
		// (karma <= 0 but above threshold) and AutoDownvote is enabled,
		// we still cast a -1 + disengage. This catches cases where karma
		// is 0/-1/-2 (not bad enough for the hard gate above) but the
		// brain confirms the post isn't worth engagement.
		if opts.AutoDownvote && !opts.DryRun && !lowQuota &&
			trig.PostID != "" && tctx.Thread != nil && tctx.Thread.Post.Karma <= 0 {
			if err := c.SkillVote(ctx, trig.PostID, -1); err != nil {
				log.Printf("[daemon] post-skip auto-downvote failed: %v", err)
				writeAudit(audit, trig, act, "skip", nil)
				return
			}
			disengage.Add(trig.PostID)
			if opts.Verbose {
				log.Printf("[daemon] auto-downvoted %s after brain skip (karma=%d)",
					trig.PostID, tctx.Thread.Post.Karma)
			}
			writeAudit(audit, trig, act, "skip-auto-downvote", nil)
			return
		}
		writeAudit(audit, trig, act, "skip", nil)
		return
	}

	if opts.DryRun {
		log.Printf("[daemon] DRY-RUN would execute: %s", summarizeAction(act))
		writeAudit(audit, trig, act, "dry-run", nil)
		return
	}

	if lowQuota {
		log.Printf("[daemon] write quota low (%d) — skipping execution this cycle", hb.RemainingQuota.WritePerMin)
		writeAudit(audit, trig, act, "quota-skip", nil)
		return
	}

	if !limiter.Try() {
		log.Printf("[daemon] local rate limit hit (%d/hour) — skipping this cycle",
			opts.MaxActionsPerHour)
		writeAudit(audit, trig, act, "rate-skip", nil)
		return
	}

	// Anti-pingpong gate: cap replies-per-post-per-hour. Only triggered
	// for "reply" actions; other actions pass through.
	if act.Type == "reply" && !postReplyLim.Try(act.PostID) {
		log.Printf("[daemon] post-reply-cap hit for post=%s (already %d replies in last hour) — skipping",
			act.PostID, opts.PostReplyCap)
		writeAudit(audit, trig, act, "post-reply-skip", nil)
		return
	}

	result, err := executeAction(ctx, c, act, api.SkillCreatePostOpts{
		AuthorModel:  opts.AuthorModel,
		AuthorClient: opts.AuthorClient,
	})
	if err != nil {
		log.Printf("[daemon] action failed: %v", err)
		writeAudit(audit, trig, act, "error", err)
		return
	}
	log.Printf("[daemon] action OK: %s", result)
	writeAudit(audit, trig, act, result, nil)

	// Record successful post for the force-post tracker. Only "post"
	// counts; replies/votes/reviews don't reset the timer.
	if act.Type == "post" {
		postTracker.RecordPost()
	}
}

// triggersContainType returns true if any trigger in the slice has the given Type.
func triggersContainType(ts []api.Trigger, typ string) bool {
	for _, t := range ts {
		if t.Type == typ {
			return true
		}
	}
	return false
}

// postTracker keeps a single timestamp: the last time this daemon
// successfully created a post. Persisted via the audit log on cold start
// (so a restart of a long-running daemon doesn't immediately re-publish).
type postTracker struct {
	lastPost time.Time
	every    time.Duration
}

// newPostTracker builds a tracker. If every == 0 the tracker is disabled
// (Overdue always returns false).
//
// On startup it tries to read the most recent "posted *" entry from the
// audit log. If found, lastPost = that timestamp. Otherwise lastPost is
// initialized to "now", giving the daemon a full ForcePostEvery window
// before its first synthetic silent_too_long fires.
func newPostTracker(auditPath string, every time.Duration) *postTracker {
	pt := &postTracker{every: every, lastPost: time.Now()}
	if every == 0 || auditPath == "" {
		return pt
	}
	if t, ok := lastPostTimeFromAudit(auditPath); ok {
		pt.lastPost = t
	}
	return pt
}

// Overdue returns true when more than `every` has elapsed since lastPost
// AND the tracker is enabled.
func (pt *postTracker) Overdue() bool {
	if pt.every == 0 {
		return false
	}
	return time.Since(pt.lastPost) > pt.every
}

// Last returns the last-post timestamp.
func (pt *postTracker) Last() time.Time { return pt.lastPost }

// RecordPost is called by runCycle after a successful post.
func (pt *postTracker) RecordPost() { pt.lastPost = time.Now() }

// lastPostTimeFromAudit scans an audit JSON-lines file backwards for the
// most recent entry whose outcome starts with "posted ". Returns the parsed
// time and true if found. Best-effort — silent on any parse error.
func lastPostTimeFromAudit(path string) (time.Time, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false
	}
	// Walk lines from last to first. Audit log appends, so iterate in reverse.
	type entry struct {
		Time    string `json:"time"`
		Outcome string `json:"outcome"`
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var e entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if !strings.HasPrefix(e.Outcome, "posted") {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, e.Time); err == nil {
			return t, true
		}
		if t, err := time.Parse(time.RFC3339, e.Time); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// fetchContext pulls the extra data the brain needs to decide well. Each
// trigger type has a different context shape; for low-priority triggers we
// keep fetches bounded (e.g. 3 posts max).
func fetchContext(ctx context.Context, c *api.Client, t api.Trigger) (*TriggerContext, error) {
	tc := &TriggerContext{}

	switch t.Type {
	case "review_due", "mention", "reply_to_me":
		if t.PostID == "" {
			return tc, nil
		}
		th, err := c.SkillGetThread(ctx, t.PostID)
		if err != nil {
			// Not fatal — brain can still decide with just the trigger text.
			return tc, nil
		}
		tc.Thread = th

	case "silent_too_long":
		subs, err := c.SkillListSubmolts(ctx)
		if err != nil {
			return tc, nil
		}
		// Cap to 10 — large lists blow up the prompt.
		if len(subs) > 10 {
			subs = subs[:10]
		}
		tc.Submolts = subs

		// Fetch discoverable tags so the brain can pick 1–3 for the new
		// post. Best-effort: failure leaves tc.Tags nil and the prompt
		// quietly skips the picker section. /skill/tags returns curated
		// (high-priority) seeds first, then local tags by post_count.
		// Cap 30 to keep the prompt under control even on busy forums.
		tags, err := c.SkillListTags(ctx, 30)
		if err == nil {
			tc.Tags = tags
		}

	case "feed_interesting":
		maxFetch := 3
		if len(t.PostIDs) < maxFetch {
			maxFetch = len(t.PostIDs)
		}
		for i := 0; i < maxFetch; i++ {
			p, err := c.GetPost(ctx, t.PostIDs[i])
			if err != nil {
				continue
			}
			tc.Posts = append(tc.Posts, *p)
		}
	}
	return tc, nil
}

// summarizeAction returns a one-line human summary for logs.
func summarizeAction(a *Action) string {
	switch a.Type {
	case "reply":
		return fmt.Sprintf("reply to %s: %q", a.PostID, firstLine(a.Content, 80))
	case "post":
		return fmt.Sprintf("post in %s: %q", a.SubMoltID, firstLine(a.Title, 80))
	case "vote":
		return fmt.Sprintf("vote %+d on %s", a.Value, a.PostID)
	case "review":
		return fmt.Sprintf("review %s score=%.1f", a.PostID, a.Score)
	case "skip":
		return "skip"
	default:
		return a.Type
	}
}

func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// ── audit log ───────────────────────────────────────────────────────────────

// openAudit opens (or creates) the JSON-lines audit log. Nil-safe: callers
// can feed nil and writeAudit becomes a no-op.
func openAudit(path string) (io.WriteCloser, error) {
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
}

type auditEntry struct {
	Time    string      `json:"time"`
	Trigger api.Trigger `json:"trigger"`
	Action  *Action     `json:"action"`
	Outcome string      `json:"outcome"`
	Error   string      `json:"error,omitempty"`
}

func writeAudit(w io.Writer, t api.Trigger, a *Action, outcome string, err error) {
	if w == nil {
		return
	}
	entry := auditEntry{
		Time:    time.Now().UTC().Format(time.RFC3339Nano),
		Trigger: t,
		Action:  a,
		Outcome: outcome,
	}
	if err != nil {
		entry.Error = err.Error()
	}
	data, jsonErr := json.Marshal(entry)
	if jsonErr != nil {
		return
	}
	_, _ = w.Write(append(data, '\n'))
}
