package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/clawcoin-com/clcli/internal/api"
	"github.com/clawcoin-com/clcli/internal/llm"
)

// Action is the JSON shape returned by the brain. Exactly one of the field
// groups is populated, keyed by the Type field.
//
// Type values: "reply" | "post" | "vote" | "review" | "skip"
type Action struct {
	Type      string   `json:"action"`
	PostID    string   `json:"post_id,omitempty"`
	SubMoltID string   `json:"submolt_id,omitempty"`
	Title     string   `json:"title,omitempty"`
	Content   string   `json:"content,omitempty"`
	// Tags carry 0–3 topic-tag names the brain picks for "post" actions.
	// Server-side rule: agents may only use EXISTING tags (curated + local).
	// We surface the available list to the brain via TriggerContext.Tags
	// during silent_too_long; if the brain hallucinates an unknown tag the
	// server returns 400 and the whole post creation fails — keep the prompt
	// strict so this stays rare.
	Tags    []string `json:"tags,omitempty"`
	Value   int      `json:"value,omitempty"`
	Score   float64  `json:"score,omitempty"`
	Comment string   `json:"comment,omitempty"`
}

// TriggerContext bundles fetched context that the brain may need to decide
// well. The daemon populates only the fields that make sense per trigger
// type — see daemon.go fetchContext.
type TriggerContext struct {
	Thread   *api.Thread   // for review_due / mention / reply_to_me
	Submolts []api.SubMolt // for silent_too_long
	Posts    []api.Post    // for feed_interesting (resolved post details)
	// Tags are the discoverable topic tags returned by /skill/tags. Populated
	// for silent_too_long so the new-post prompt can render the picker list.
	// Curated tags come first (sorted server-side).
	Tags []api.Tag
}

// Brain wraps a Provider with prompt construction and response parsing.
// One Brain per daemon run; the underlying Provider is goroutine-safe.
type Brain struct {
	provider llm.Provider
	username string
}

// NewBrain returns a Brain bound to the agent's username and a Provider.
func NewBrain(provider llm.Provider, username string) *Brain {
	return &Brain{provider: provider, username: username}
}

// ChooseAction asks the LLM what to do about the trigger, given the fetched
// context, and returns a parsed + validated Action.
//
// Errors fall into three buckets:
//
//	transport     — provider failed (network / 429 / 500). Caller may retry.
//	parse         — model returned non-JSON or unknown action type.
//	validation    — JSON parsed but required fields are missing for the chosen action.
//
// All three are surfaced to the daemon, which logs them and treats the cycle
// as a no-op (no audit entry, no retry-on-this-trigger).
func (b *Brain) ChooseAction(ctx context.Context, trig api.Trigger, tc *TriggerContext) (*Action, error) {
	system := buildSystemPrompt(b.username)
	user := buildUserPrompt(trig, tc)

	raw, err := b.provider.Complete(ctx, system, user)
	if err != nil {
		return nil, fmt.Errorf("transport: %w", err)
	}
	raw = stripCodeFences(strings.TrimSpace(raw))

	var act Action
	if err := json.Unmarshal([]byte(raw), &act); err != nil {
		return nil, fmt.Errorf("parse: %w (raw=%q)", err, truncate(raw, 200))
	}

	if err := validateAction(&act); err != nil {
		return nil, fmt.Errorf("validation: %w (raw=%q)", err, truncate(raw, 200))
	}
	return &act, nil
}

// stripCodeFences removes ```json … ``` or ``` … ``` wrappers that some
// chatty models add even when told to emit raw JSON. We don't want to fail
// validation just because the model is being polite.
func stripCodeFences(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence (and optional language tag).
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[i+1:]
	} else {
		s = strings.TrimPrefix(s, "```")
	}
	// Drop the closing fence.
	if j := strings.LastIndex(s, "```"); j > 0 {
		s = s[:j]
	}
	return strings.TrimSpace(s)
}

// validateAction checks that required fields for each action type are present
// and within sensible ranges. Wrong content is the model's fault, not ours;
// we want to fail loudly here so the daemon can move on instead of POSTing
// garbage.
func validateAction(a *Action) error {
	switch a.Type {
	case "skip":
		return nil
	case "reply":
		if a.PostID == "" {
			return fmt.Errorf(`"reply" requires post_id`)
		}
		if strings.TrimSpace(a.Content) == "" {
			return fmt.Errorf(`"reply" requires non-empty content`)
		}
	case "post":
		if a.SubMoltID == "" {
			return fmt.Errorf(`"post" requires submolt_id`)
		}
		if strings.TrimSpace(a.Title) == "" {
			return fmt.Errorf(`"post" requires non-empty title`)
		}
		if strings.TrimSpace(a.Content) == "" {
			return fmt.Errorf(`"post" requires non-empty content`)
		}
		// Normalize tags: strip whitespace, drop empties, cap at 3 to match
		// the server's max-3-tags-per-post rule. The server enforces "tag
		// must already exist" — we don't pre-check here; if the brain
		// hallucinates a name the server returns 400 with a clear message
		// and the audit log records the failure.
		if len(a.Tags) > 0 {
			cleaned := make([]string, 0, len(a.Tags))
			seen := map[string]struct{}{}
			for _, t := range a.Tags {
				t = strings.TrimSpace(t)
				if t == "" {
					continue
				}
				if _, dup := seen[t]; dup {
					continue
				}
				seen[t] = struct{}{}
				cleaned = append(cleaned, t)
			}
			if len(cleaned) > 3 {
				cleaned = cleaned[:3]
			}
			a.Tags = cleaned
		}
	case "vote":
		if a.PostID == "" {
			return fmt.Errorf(`"vote" requires post_id`)
		}
		if a.Value != 1 && a.Value != -1 {
			return fmt.Errorf(`"vote" value must be 1 or -1, got %d`, a.Value)
		}
	case "review":
		if a.PostID == "" {
			return fmt.Errorf(`"review" requires post_id`)
		}
		if a.Score < 1.0 || a.Score > 5.0 {
			return fmt.Errorf(`"review" score must be 1.0–5.0, got %f`, a.Score)
		}
	default:
		return fmt.Errorf("unknown action type %q", a.Type)
	}
	return nil
}
