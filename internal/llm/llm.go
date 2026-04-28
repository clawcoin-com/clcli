// Package llm abstracts the language-model backend used by `clcli agent run`.
//
// The daemon's brain never calls a provider SDK directly — it calls
// Provider.Complete(ctx, system, user) and parses the JSON response.
// Adding a new backend means writing one file that satisfies the interface.
//
// Two providers ship in-box:
//
//   - openai     chat/completions wire format. Works with api.openai.com
//                plus any OpenAI-compatible endpoint (LiteLLM, Ollama,
//                OpenRouter, Azure OpenAI, vLLM, llama.cpp) when
//                APIBaseURL is pointed at that server.
//   - anthropic  messages API (claude.ai).
//
// The default base URL points at a local LiteLLM gateway
// (http://127.0.0.1:4000/v1), matching cccli's convention: operators run
// one LiteLLM instance per host and every ClawCoin CLI connects to it.
// IPv4 literal instead of "localhost" to avoid Go's IPv6 preference causing
// connection failures on Windows + Docker Desktop.
package llm

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Options bundles all knobs the provider constructors care about. Decouples
// this package from config.Config so tests / callers can pass literals.
type Options struct {
	Provider    string  // "openai" (default) or "anthropic"
	APIBaseURL  string  // "" → provider-specific default
	APIKey      string  // from CLCLI_LLM_API_KEY
	Model       string  // "" → provider-specific default
	MaxTokens   int     // 0 → no cap (provider default). cccli uses 4096.
	Temperature float64 // 0 → provider default (usually ~0.7)
	Thinking    bool    // false → ask model to skip <think> reasoning
}

// Provider is the single point of integration with any LLM backend.
//
// Complete is a one-shot "reason about this trigger" call. The daemon does
// NOT keep conversation history across triggers — every trigger is a
// fresh context, which keeps state cheap and bounded.
//
// Implementations MUST post-process the returned text with StripThinkTags
// (e.g. via the helper in this file) so models that leak <think> blocks
// don't poison downstream JSON parsing.
type Provider interface {
	Complete(ctx context.Context, system, user string) (string, error)
	Name() string
	Model() string
}

// New returns the Provider configured by opts, or an error if unusable
// (missing key, unknown provider, etc.).
func New(opts Options) (Provider, error) {
	provider := strings.ToLower(strings.TrimSpace(opts.Provider))
	if provider == "" {
		provider = "openai"
	}
	if opts.APIKey == "" {
		return nil, errors.New("LLM API key is empty — set CLCLI_LLM_API_KEY")
	}

	switch provider {
	case "openai", "openai-compatible":
		return newOpenAI(opts), nil
	case "anthropic":
		return newAnthropic(opts), nil
	default:
		return nil, fmt.Errorf("unknown llm provider %q (want openai | anthropic)", provider)
	}
}

// Ping issues a minimal probe completion to confirm the provider is
// reachable, the API key is valid, and the model name is accepted.
// Wrapped with a short timeout so a misconfigured endpoint fails fast
// instead of hanging the daemon startup for 90 seconds.
func Ping(ctx context.Context, p Provider) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := p.Complete(ctx, "You are a health-check probe.", "Reply with the single word: pong")
	return err
}

// ── Response sanitization ───────────────────────────────────────────────

// thinkTagRe matches <think>...</think> blocks emitted by reasoning models
// (Qwen3, DeepSeek-R1, etc.) even when enable_thinking is set to false.
// The (?s) flag makes `.` match newlines.
var thinkTagRe = regexp.MustCompile(`(?s)<think>.*?</think>\s*`)

// StripThinkTags removes any <think>...</think> blocks and trims surrounding
// whitespace. Provider implementations call this on every response so the
// daemon brain always receives clean output. Safe to call on text with no
// think tags — returns the input (trimmed) unchanged.
func StripThinkTags(s string) string {
	s = thinkTagRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}
