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

// openaiClient talks to any OpenAI-shaped chat/completions endpoint. This
// covers api.openai.com, LiteLLM gateway, Azure OpenAI, OpenRouter, Ollama,
// self-hosted vLLM / llama.cpp — anything implementing /chat/completions.
type openaiClient struct {
	apiKey      string
	baseURL     string // no trailing slash; /chat/completions is appended
	model       string
	maxTokens   int
	temperature float64
	thinking    bool
	httpClient  *http.Client
}

func newOpenAI(opts Options) *openaiClient {
	base := opts.APIBaseURL
	if base == "" {
		// LiteLLM gateway default — matches cccli's deployment assumption.
		// 127.0.0.1 (not localhost) avoids an IPv6 preference pitfall on
		// Windows + Docker Desktop. See config.LLMAPIBaseURL doc for detail.
		// Users who want direct OpenAI.com should set CLCLI_LLM_API_BASE_URL
		// to https://api.openai.com/v1.
		base = "http://127.0.0.1:4000/v1"
	}
	base = strings.TrimRight(base, "/")

	model := opts.Model
	if model == "" {
		model = "gpt-4o-mini"
	}

	return &openaiClient{
		apiKey:      opts.APIKey,
		baseURL:     base,
		model:       model,
		maxTokens:   opts.MaxTokens,
		temperature: opts.Temperature,
		thinking:    opts.Thinking,
		httpClient: &http.Client{
			Timeout: 90 * time.Second,
			Transport: &http.Transport{
				// Some proxies (LiteLLM on certain configs, Nginx with
				// specific gzip settings) hang during gzip negotiation;
				// disabling compression avoids the dead lock and costs
				// little on 4KB chat responses.
				DisableCompression: true,
			},
		},
	}
}

func (c *openaiClient) Name() string  { return "openai" }
func (c *openaiClient) Model() string { return c.model }

type openaiRequest struct {
	Model          string          `json:"model"`
	Messages       []openaiMessage `json:"messages"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	EnableThinking *bool           `json:"enable_thinking,omitempty"` // Qwen3 / DeepSeek-R1
}

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiResponse struct {
	Choices []struct {
		Message openaiMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func (c *openaiClient) Complete(ctx context.Context, system, user string) (string, error) {
	reqBody := openaiRequest{
		Model: c.model,
		Messages: []openaiMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		MaxTokens: c.maxTokens,
	}
	if c.temperature > 0 {
		t := c.temperature
		reqBody.Temperature = &t
	}
	// Disable thinking/reasoning for models that support it (Qwen3,
	// DeepSeek-R1, etc.) unless operator explicitly enabled it. This
	// significantly reduces latency AND keeps JSON output clean.
	if !c.thinking {
		f := false
		reqBody.EnableThinking = &f
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	url := c.baseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("openai: %w", err)
	}
	defer resp.Body.Close()

	// 10 MB response ceiling — prevents a runaway / malicious endpoint from
	// chewing through memory. Cap matches cccli.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 10<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var parsed openaiResponse
		_ = json.Unmarshal(raw, &parsed)
		if parsed.Error != nil && parsed.Error.Message != "" {
			return "", fmt.Errorf("openai %d: %s", resp.StatusCode, parsed.Error.Message)
		}
		return "", fmt.Errorf("openai %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	var parsed openaiResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("openai: parse response: %w (body=%s)", err, truncate(string(raw), 300))
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("openai: empty choices array (body=%s)", truncate(string(raw), 300))
	}
	content := parsed.Choices[0].Message.Content
	if content == "" {
		return "", fmt.Errorf("openai: empty content (model may need higher max_tokens or does not support non-thinking mode)")
	}
	// Always strip <think>...</think> — some models leak them even when
	// enable_thinking=false is sent.
	return StripThinkTags(content), nil
}

// truncate returns s truncated to maxLen bytes. Used only for error messages.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}
