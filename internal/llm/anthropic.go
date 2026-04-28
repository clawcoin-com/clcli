package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// anthropicClient talks to the Anthropic messages API. Differences from
// the OpenAI wire shape: the system prompt is a top-level field (not a
// message), responses use `content` blocks, header auth uses x-api-key.
type anthropicClient struct {
	apiKey      string
	baseURL     string
	model       string
	maxTokens   int
	temperature float64
	httpClient  *http.Client
}

func newAnthropic(opts Options) *anthropicClient {
	base := opts.APIBaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	base = strings.TrimRight(base, "/")

	model := opts.Model
	if model == "" {
		model = "claude-3-5-sonnet-20241022"
	}

	return &anthropicClient{
		apiKey:      opts.APIKey,
		baseURL:     base,
		model:       model,
		maxTokens:   opts.MaxTokens,
		temperature: opts.Temperature,
		httpClient: &http.Client{
			Timeout: 90 * time.Second,
			Transport: &http.Transport{
				DisableCompression: true,
			},
		},
	}
}

func (c *anthropicClient) Name() string  { return "anthropic" }
func (c *anthropicClient) Model() string { return c.model }

type anthropicRequest struct {
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"` // required by Anthropic
	Temperature *float64           `json:"temperature,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponseBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicResponse struct {
	Content []anthropicResponseBlock `json:"content"`
	Error   *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *anthropicClient) Complete(ctx context.Context, system, user string) (string, error) {
	maxTokens := c.maxTokens
	if maxTokens <= 0 {
		// Anthropic requires max_tokens; 4096 matches the default ceiling
		// cccli uses for JSON-returning prompts.
		maxTokens = 4096
	}

	reqBody := anthropicRequest{
		Model:     c.model,
		System:    system,
		Messages:  []anthropicMessage{{Role: "user", Content: user}},
		MaxTokens: maxTokens,
	}
	if c.temperature > 0 {
		t := c.temperature
		reqBody.Temperature = &t
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	url := c.baseURL + "/v1/messages"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var parsed anthropicResponse
		_ = json.Unmarshal(raw, &parsed)
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", fmt.Errorf("anthropic %d: %s", resp.StatusCode, parsed.Error.Message)
		}
		return "", fmt.Errorf("anthropic %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	var parsed anthropicResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("anthropic: parse response: %w (body=%s)", err, truncate(string(raw), 300))
	}

	// Concatenate all text blocks. With our one-shot prompt we expect a
	// single block, but defensive stacking is cheap.
	var sb strings.Builder
	for _, b := range parsed.Content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	if sb.Len() == 0 {
		return "", fmt.Errorf("anthropic: empty text response (body=%s)", truncate(string(raw), 300))
	}
	return StripThinkTags(sb.String()), nil
}
