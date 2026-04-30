package daemon

import (
	"strings"
	"testing"

	"github.com/clawcoin-com/clcli/internal/api"
)

// Verifies the v0.4 tag-aware prompt change: silent_too_long must surface
// the available tag list to the brain, and the system prompt must advertise
// the "tags" field on the post action shape.
//
// Without these, the LLM has no signal to set Action.Tags, which is exactly
// the state that produced 19/20 tagless posts in the wild before this fix.
func TestSystemPromptAdvertisesTagsField(t *testing.T) {
	got := buildSystemPrompt("test_agent")
	if !strings.Contains(got, `"tags":["<name>", ...]`) {
		t.Fatal(`system prompt missing "tags":["<name>", ...] in the post action example`)
	}
	if !strings.Contains(got, `Pick 1`) || !strings.Contains(got, `tags`) {
		t.Fatal("system prompt missing tag-selection guideline")
	}
}

func TestSilentTooLongPromptInjectsTagList(t *testing.T) {
	trig := api.Trigger{
		Type:           "silent_too_long",
		Priority:       "medium",
		ThresholdHours: 1,
	}
	tc := &TriggerContext{
		Submolts: []api.SubMolt{{ID: "sm1", Name: "general"}},
		Tags: []api.Tag{
			{Name: "AI Safety", IsCurated: true, Description: "Alignment & guardrails"},
			{Name: "ai-agents", IsCurated: false},
			{Name: "tooling", IsCurated: false},
		},
	}

	got := buildUserPrompt(trig, tc)

	for _, want := range []string{
		"Available topic tags",
		"AI Safety",
		"ai-agents",
		"tooling",
		"[curated]",
		`"tags":["<name>", ...]`,
		"copied verbatim",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("silent_too_long prompt missing %q\n--- prompt ---\n%s", want, got)
		}
	}
}

func TestSilentTooLongPromptOmitsTagSectionWhenEmpty(t *testing.T) {
	trig := api.Trigger{Type: "silent_too_long", Priority: "medium", ThresholdHours: 1}
	tc := &TriggerContext{Submolts: []api.SubMolt{{ID: "sm1", Name: "g"}}}

	got := buildUserPrompt(trig, tc)
	if strings.Contains(got, "Available topic tags") {
		t.Fatal("tag picker section should not appear when ctx.Tags is empty")
	}
	// But the Decide block still mentions tags as a valid optional field.
	if !strings.Contains(got, `"tags"`) {
		t.Fatal("Decide block must still mention tags field even when no tags are listed")
	}
}

func TestValidateActionNormalizesTags(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"trim whitespace", []string{"  ai  ", " safety"}, []string{"ai", "safety"}},
		{"drop empties", []string{"", "  ", "ai"}, []string{"ai"}},
		{"dedupe", []string{"ai", "ai", "tools"}, []string{"ai", "tools"}},
		{"cap at 3", []string{"a", "b", "c", "d", "e"}, []string{"a", "b", "c"}},
		{"all-empty becomes empty", []string{"", "  "}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Action{
				Type: "post", SubMoltID: "sm1", Title: "t", Content: "c", Tags: tt.in,
			}
			if err := validateAction(a); err != nil {
				t.Fatalf("validateAction: %v", err)
			}
			if !sliceEq(a.Tags, tt.want) {
				t.Errorf("got %v, want %v", a.Tags, tt.want)
			}
		})
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
