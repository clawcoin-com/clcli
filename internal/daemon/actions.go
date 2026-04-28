package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/clawcoin-com/clcli/internal/api"
)

// executeAction dispatches one Action through the API client. Returns a
// short human-readable result line plus the error (if any). The result line
// is what the daemon logs to the audit trail.
//
// For "reply", we always go through the queue (take → submit). That is the
// ONLY legal way for an agent to reply per the backend rules, and it also
// lets us surface a queue position in the audit log.
func executeAction(ctx context.Context, c *api.Client, act *Action) (string, error) {
	switch act.Type {
	case "skip":
		return "skipped", nil

	case "reply":
		slot, err := c.SkillQueueTake(ctx, act.PostID)
		if err != nil {
			return "", fmt.Errorf("queue/take: %w", err)
		}
		r, err := c.SkillQueueSubmit(ctx, slot.Token, act.Content, nil)
		if err != nil {
			// One retry on INVALID_TOKEN matches clcli agent reply --force semantics.
			var apiErr *api.APIError
			if errors.As(err, &apiErr) && apiErr.Code == "INVALID_TOKEN" {
				slot, err = c.SkillQueueTake(ctx, act.PostID)
				if err != nil {
					return "", fmt.Errorf("queue/take (retry): %w", err)
				}
				r, err = c.SkillQueueSubmit(ctx, slot.Token, act.Content, nil)
			}
			if err != nil {
				return "", fmt.Errorf("queue/submit: %w", err)
			}
		}
		return fmt.Sprintf("replied reply_id=%s queue_pos=%d", r.ID, slot.Position), nil

	case "post":
		p, err := c.SkillCreatePost(ctx, act.SubMoltID, act.Title, act.Content, "", nil)
		if err != nil {
			return "", fmt.Errorf("skill/posts: %w", err)
		}
		return fmt.Sprintf("posted post_id=%s", p.ID), nil

	case "vote":
		if err := c.SkillVote(ctx, act.PostID, act.Value); err != nil {
			return "", fmt.Errorf("skill/vote: %w", err)
		}
		return fmt.Sprintf("voted post_id=%s value=%d", act.PostID, act.Value), nil

	case "review":
		if err := c.SkillSubmitReview(ctx, act.PostID, act.Score, act.Comment); err != nil {
			return "", fmt.Errorf("skill/reviews: %w", err)
		}
		return fmt.Sprintf("reviewed post_id=%s score=%.1f", act.PostID, act.Score), nil

	default:
		return "", fmt.Errorf("unknown action type %q (validateAction should have caught this)", act.Type)
	}
}
