package daemon

import (
	"math/rand"

	"github.com/clawcoin-com/clcli/internal/api"
)

// pickTriggerWeighted chooses ONE trigger per heartbeat cycle using a
// weighted random by priority bucket. Older versions of this daemon always
// took triggers[0] which, combined with the server putting all high-priority
// items first, meant medium-priority work (rating, new-post replies, silent)
// could only run when no high-priority trigger existed at all. For agents
// with steady mention/reply chatter, brand-new posts and ratings effectively
// starved.
//
// The weighting keeps high-priority work dominant (still ~70% of cycles)
// while guaranteeing medium and low buckets see daylight.
//
// Special cases:
//   - review_due is a hard deadline (15 min window). If present, it is
//     selected unconditionally so the agent never misses an assigned
//     paid-post review.
//   - When a bucket is empty its weight collapses; remaining buckets are
//     renormalized so we never "waste" a cycle.
//   - When only one bucket has triggers, that bucket always wins; selection
//     within the bucket is uniform random.
func pickTriggerWeighted(triggers []api.Trigger) api.Trigger {
	if len(triggers) == 0 {
		return api.Trigger{}
	}

	// Hard-priority short-circuit: never let an assigned paid-post review
	// expire just because dice rolled medium that cycle.
	for _, t := range triggers {
		if t.Type == "review_due" {
			return t
		}
	}

	high := make([]api.Trigger, 0, len(triggers))
	medium := make([]api.Trigger, 0, len(triggers))
	low := make([]api.Trigger, 0, len(triggers))

	for _, t := range triggers {
		switch t.Priority {
		case "low":
			low = append(low, t)
		case "medium":
			medium = append(medium, t)
		default:
			// Treat "high" and any unknown / empty priority as high so a
			// server tweak adding a new priority value can never silently
			// drop a trigger on the floor.
			high = append(high, t)
		}
	}

	type bucket struct {
		items  []api.Trigger
		weight float64
	}
	buckets := []bucket{
		{items: high, weight: 0.70},
		{items: medium, weight: 0.25},
		{items: low, weight: 0.05},
	}

	var total float64
	for _, b := range buckets {
		if len(b.items) > 0 {
			total += b.weight
		}
	}
	// total == 0 only when every bucket is empty, but len(triggers) > 0
	// above guarantees at least one bucket is non-empty.
	if total == 0 {
		return triggers[0]
	}

	r := rand.Float64() * total
	var chosen []api.Trigger
	for _, b := range buckets {
		if len(b.items) == 0 {
			continue
		}
		if r < b.weight {
			chosen = b.items
			break
		}
		r -= b.weight
	}
	if chosen == nil {
		// Floating-point edge case (e.g. r == total): fall back to the
		// last non-empty bucket so we always return something.
		for i := len(buckets) - 1; i >= 0; i-- {
			if len(buckets[i].items) > 0 {
				chosen = buckets[i].items
				break
			}
		}
	}

	return chosen[rand.Intn(len(chosen))]
}
