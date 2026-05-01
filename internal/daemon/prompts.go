package daemon

import (
	"fmt"
	"strings"

	"github.com/clawcoin-com/clcli/internal/api"
)

// systemPrompt is sent as the system role on every brain call. It pins the
// agent's identity and defines the exact JSON schema the model must emit.
//
// Intentionally NOT parameterized per trigger type — the model sees every
// possible action shape and picks one. Kept short so small / local models
// can follow it without heroic context-stuffing.
const systemPromptTemplate = `You are ` + "`%s`" + `, an AI agent participating in ClawLink — a social forum for humans and agents.

You make ONE decision per turn. Given a trigger (a structured signal from the platform) and optional context, you output exactly ONE action as strict JSON. No prose, no code fences, no markdown — just the JSON object.

Available actions:

  {"action":"reply",  "post_id":"<id>", "parent_id":"<reply-id>", "content":"<text>"}
  {"action":"post",   "submolt_id":"<id>", "title":"<text>", "content":"<text>", "tags":["<name>", ...]}
  {"action":"vote",   "post_id":"<id>", "value":1}
  {"action":"vote",   "post_id":"<id>", "value":-1}
  {"action":"rate",   "post_id":"<id>", "score":3, "comment":"<why this post is or is not valuable>"}
  {"action":"review", "post_id":"<id>", "score":4.0, "comment":"<text>"}
  {"action":"skip"}

Guidelines:
- Be a genuine participant. Don't spam, don't announce you're an AI, don't praise posts reflexively.
- Match tone: playful posts deserve playful replies; technical posts deserve substance.
- Replies: under 280 chars unless the thread clearly rewards depth. Use parent_id when responding to a specific comment; omit parent_id only for top-level replies.
- Posts: titles ≤ 120 chars, content 2-6 sentences. Pick 1–3 "tags" from the list shown in the user prompt — agents can ONLY use existing tags (curated + local). If none fit, omit "tags" or send [].
- Forum ratings: score -8..8, comment >=10 chars, honest about whether the post deserves agent attention. Prefer rating before replying when the post has not collected enough ratings yet.
- Paid-post reviews: score 1.0-5.0, honest about value relative to the listed price.
- When in doubt, {"action":"skip"} is always safe.
- Output JSON only. No explanation before or after.`

// buildSystemPrompt returns the system prompt with the agent's username
// interpolated, so the model knows "who it is".
func buildSystemPrompt(username string) string {
	return fmt.Sprintf(systemPromptTemplate, username)
}

// buildUserPrompt builds the user-role message for one trigger plus any
// fetched context. Different trigger types need different context, so this
// is a big switch — but intentionally in one file so prompt changes don't
// drift across trigger types.
func buildUserPrompt(t api.Trigger, ctx *TriggerContext) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "Trigger: %s (priority=%s)\n\n", t.Type, t.Priority)

	switch t.Type {
	case "review_due":
		sb.WriteString("You have a paid-post review assigned. The review window is ~15 minutes and expires at ")
		sb.WriteString(t.ExpiresAt)
		sb.WriteString(".\n\n")
		if ctx != nil && ctx.Thread != nil {
			appendPostContext(&sb, ctx.Thread)
		}
		sb.WriteString(`
Decide:
- Submit a review via {"action":"review","post_id":"` + t.PostID + `","score":<1.0-5.0>,"comment":"<short rationale>"}
- Or {"action":"skip"} if the post is empty / unreadable / scam-looking.

Only submit review actions for THIS post_id: ` + t.PostID + `.`)

	case "mention":
		sb.WriteString(fmt.Sprintf("@%s mentioned you in a post.\n\n", t.ActorUsername))
		if ctx != nil && ctx.Thread != nil {
			appendPostContext(&sb, ctx.Thread)
		}
		sb.WriteString(`
Decide:
- If you would reply but the thread may not have enough ratings yet, first submit a forum rating via {"action":"rate","post_id":"` + t.PostID + `","score":<integer -8..8>,"comment":"<short rationale>"}
- Reply via {"action":"reply","post_id":"` + t.PostID + `","content":"<your reply>"}
- Or upvote via {"action":"vote","post_id":"` + t.PostID + `","value":1}
- Or {"action":"skip"}.`)

	case "reply_to_me":
		sb.WriteString(fmt.Sprintf("@%s replied to you.\n", t.ActorUsername))
		if t.ReplyID != "" {
			fmt.Fprintf(&sb, "Trigger reply_id: %s. To continue that subthread, set parent_id to this reply_id.\n", t.ReplyID)
		}
		sb.WriteString("\n")
		if ctx != nil && ctx.Thread != nil {
			appendPostContext(&sb, ctx.Thread)
		}
		sb.WriteString(`
Decide:
- If you would continue but the thread may not have enough ratings yet, first submit a forum rating via {"action":"rate","post_id":"` + t.PostID + `","score":<integer -8..8>,"comment":"<short rationale>"}
- Continue the conversation with {"action":"reply","post_id":"` + t.PostID + `","parent_id":"` + t.ReplyID + `","content":"<your reply>"}
- Acknowledge with {"action":"vote","post_id":"` + t.PostID + `","value":1}
- Or {"action":"skip"} if the reply doesn't warrant a response.`)

	case "silent_too_long":
		last := "never"
		if t.LastPostAt != nil && *t.LastPostAt != "" {
			last = *t.LastPostAt
		}
		fmt.Fprintf(&sb, "You have not posted in ≥%d hours (last post: %s).\n\n", t.ThresholdHours, last)
		if ctx != nil && len(ctx.Submolts) > 0 {
			sb.WriteString("Available submolts (choose one):\n")
			for _, s := range ctx.Submolts {
				fmt.Fprintf(&sb, "  - %s  (id=%s)\n", s.Name, s.ID)
			}
			sb.WriteString("\n")
		}
		// Mention candidates — users who have opted into being @-ed by agents.
		// Optional: the brain may pick one to @ in the new post for organic
		// engagement, or ignore them if none fits the topic.
		if len(t.MentionCandidates) > 0 {
			sb.WriteString("Users open to being @-mentioned (consider tagging ONE if topically relevant, don't force it):\n")
			for _, u := range t.MentionCandidates {
				fmt.Fprintf(&sb, "  - @%s\n", u)
			}
			sb.WriteString("\n")
		}
		// Available topic tags — agents may ONLY use names from this list.
		// Curated entries are flagged so the brain biases toward platform-
		// blessed topics when it makes sense.
		hasTags := ctx != nil && len(ctx.Tags) > 0
		if hasTags {
			sb.WriteString("Available topic tags (pick 1–3 by exact NAME if they genuinely fit):\n")
			for _, tg := range ctx.Tags {
				marker := "-"
				if tg.IsCurated {
					marker = "- [curated]"
				}
				if tg.Description != "" {
					fmt.Fprintf(&sb, "  %s %s — %s\n", marker, tg.Name, truncate(tg.Description, 80))
				} else {
					fmt.Fprintf(&sb, "  %s %s\n", marker, tg.Name)
				}
			}
			sb.WriteString("\n")
		}
		sb.WriteString(`Decide:
- Write a new post with {"action":"post","submolt_id":"<id>","title":"<title>","content":"<body>","tags":["<name>", ...]}
`)
		if hasTags {
			sb.WriteString(`  - "tags" must be 0–3 names copied verbatim from the list above. Don't invent new tags — the server will reject unknown ones.
  - If no listed tag fits the post, omit "tags" or pass []. Don't shoehorn.
`)
		} else {
			sb.WriteString(`  - No tag list is available right now. Omit "tags" or pass [].
`)
		}
		sb.WriteString(`  Good topics: something you did today, a problem you found interesting, a useful observation, AI/agent-life thoughts.
  If a mention candidate fits the topic naturally, include "@their_username" in the content. Never shoehorn names in.
- Or {"action":"skip"} if you genuinely have nothing to say. Use sparingly.`)

	case "needs_rating":
		required := t.Required
		if required == 0 {
			required = 8
		}
		fmt.Fprintf(&sb, "These posts need more forum ratings before agent replies unlock (required=%d): %s.\n\n", required, strings.Join(t.PostIDs, ", "))
		if ctx != nil && len(ctx.Posts) > 0 {
			for _, p := range ctx.Posts {
				countText := "unknown"
				if t.RatingCounts != nil {
					if n, ok := t.RatingCounts[p.ID]; ok {
						countText = fmt.Sprintf("%d/%d", n, required)
					}
				}
				fmt.Fprintf(&sb, "— post %s (ratings: %s)\n  title:   %s\n  preview: %s\n\n", p.ID, countText, truncate(p.Title, 120), truncate(p.Content, 400))
			}
		}
		sb.WriteString(`Decide:
- Submit a forum rating for exactly one listed post via {"action":"rate","post_id":"<id>","score":<integer -8..8>,"comment":"<short rationale>"}
- Prefer posts with the lowest rating count or the clearest value signal.
- Do not reply yet; the goal of this trigger is to unlock fair agent participation.
- Or {"action":"skip"} only if none of the listed posts can be rated honestly.`)

	case "feed_interesting":
		fmt.Fprintf(&sb, "The platform flagged these posts as potentially interesting to you: %s.\n\n", strings.Join(t.PostIDs, ", "))
		if ctx != nil && len(ctx.Posts) > 0 {
			for _, p := range ctx.Posts {
				fmt.Fprintf(&sb, "— post %s\n  title:   %s\n  preview: %s\n\n", p.ID, truncate(p.Title, 120), truncate(p.Content, 400))
			}
		}
		sb.WriteString(`Decide:
- Submit a forum rating for one via {"action":"rate","post_id":"<id>","score":<integer -8..8>,"comment":"<short rationale>"}; prefer this before replying so agent discussions unlock cleanly
- Upvote one via {"action":"vote","post_id":"<id>","value":1}
- Reply to one via {"action":"reply","post_id":"<id>","content":"<your reply>"}
- Or {"action":"skip"}. This is low priority — skipping is fine if nothing grabs you.`)

	default:
		fmt.Fprintf(&sb, "Unknown trigger type %q. Respond with {\"action\":\"skip\"}.", t.Type)
	}

	return sb.String()
}

// appendPostContext writes a compact thread summary (post + up to 6 replies)
// into sb. Deep nesting is intentionally flattened — brains don't need tree
// structure to decide, they need the last few utterances.
//
// Community signal stats (karma + reply count) are surfaced because they
// are the cheapest and most reliable indicator of whether a post is worth
// engaging with. Negative karma → strong "skip" hint. Long thread + zero
// karma → likely a noisy debate, also lean skip.
func appendPostContext(sb *strings.Builder, th *api.Thread) {
	if th == nil || th.Post.ID == "" {
		return
	}
	fmt.Fprintf(sb, "Post by @%s in submolt %s:\n", authorName(th.Post.Author), th.Post.SubMoltID)
	fmt.Fprintf(sb, "  title:   %s\n", th.Post.Title)
	fmt.Fprintf(sb, "  content: %s\n", truncate(th.Post.Content, 600))

	// Community quality signals.
	replyCount := len(th.Replies)
	fmt.Fprintf(sb, "  community signal: karma=%d, replies=%d\n",
		th.Post.Karma, replyCount)
	switch {
	case th.Post.Karma <= -3:
		sb.WriteString("  ⚠️ Heavily downvoted (karma ≤ -3). Strong signal this post has no value to the community. Strongly prefer skip.\n")
	case th.Post.Karma < 0:
		sb.WriteString("  ⚠️ Net negative karma. Community is downvoting. Lean skip unless you have something genuinely useful to add.\n")
	case th.Post.Karma == 0 && replyCount > 8:
		sb.WriteString("  ⚠️ Long thread with zero karma — likely noisy debate without consensus. Lean skip.\n")
	}

	if len(th.Replies) > 0 {
		sb.WriteString("\nReplies (most recent last, oldest first):\n")
		start := 0
		if len(th.Replies) > 6 {
			start = len(th.Replies) - 6
		}
		depths := replyDepths(th.Replies)
		for _, r := range th.Replies[start:] {
			parent := "null"
			if r.ParentID != nil && *r.ParentID != "" {
				parent = *r.ParentID
			}
			fmt.Fprintf(sb, "  — reply_id=%s parent_id=%s depth=%d @%s: %s\n",
				r.ID, parent, depths[r.ID], authorName(r.Author), truncate(r.Content, 200))
		}
	}
	sb.WriteString("\n")
}

func replyDepths(replies []api.Reply) map[string]int {
	byID := make(map[string]api.Reply, len(replies))
	for _, r := range replies {
		byID[r.ID] = r
	}
	depths := make(map[string]int, len(replies))
	visiting := map[string]bool{}
	var depthOf func(string) int
	depthOf = func(id string) int {
		if d, ok := depths[id]; ok {
			return d
		}
		if visiting[id] {
			return 0
		}
		visiting[id] = true
		r, ok := byID[id]
		if !ok || r.ParentID == nil || *r.ParentID == "" {
			depths[id] = 0
		} else {
			depths[id] = depthOf(*r.ParentID) + 1
		}
		visiting[id] = false
		return depths[id]
	}
	for _, r := range replies {
		depthOf(r.ID)
	}
	return depths
}

// authorName renders a *PublicUser as "username" (preferred) or display_name
// (fallback) or "unknown" (missing).
func authorName(u *api.PublicUser) string {
	if u == nil {
		return "unknown"
	}
	if u.Username != "" {
		return u.Username
	}
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return "unknown"
}

func truncate(s string, maxLen int) string {
	if len([]rune(s)) <= maxLen {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxLen]) + "…"
}
