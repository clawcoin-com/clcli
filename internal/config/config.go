// Package config provides minimal YAML + env-var configuration management.
//
// We intentionally avoid viper/cobra-viper to keep the binary small. Loading
// semantics: defaults → YAML file → CLCLI_* env vars (highest priority).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultProfile is the profile name used when --profile is not supplied.
const DefaultProfile = "clagent"

// profileNamePattern restricts profile names to a safe, filesystem-friendly
// subset so a profile value can never traverse outside the profiles/ directory.
var profileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// Config holds all configuration for clcli.
//
// LLM fields are flat (llm_*) to match the sibling cccli layout. When config
// shapes diverge, cross-tool knowledge (env vars, key names) stops
// transferring and people get confused. Prefer cccli's vocabulary.
type Config struct {
	// ClawLink REST API base (no trailing slash). Public default uses the main
	// production API; local development can override this via config or env.
	APIBaseURL string `yaml:"api_base_url"`

	// Local data directory (stores keystore, session, etc.). Runtime-computed
	// from the profile name; intentionally NOT serialized to YAML so that
	// legacy config files on disk cannot override the profile-scoped path.
	HomeDir string `yaml:"-"`

	// ClawCoin mainnet/public config by default. Testnet can be selected by
	// overriding chain_id and rpc_url in config or env.
	ChainID int64  `yaml:"chain_id"` // 11111111
	RPCURL  string `yaml:"rpc_url"`  // https://evm.clawcoin.com
	Denom   string `yaml:"denom"`    // CC (display); wei on-chain

	// Gas defaults.
	GasLimit uint64 `yaml:"gas_limit"`
	GasPrice string `yaml:"gas_price"` // wei, decimal

	// ── LLM configuration (consumed by `clcli agent run`) ──────────────────
	//
	// Provider selects the wire format: "openai" (default) or "anthropic".
	// "openai" also works for any OpenAI-compatible backend (LiteLLM,
	// Ollama, OpenRouter, Azure OpenAI, vLLM, llama.cpp server, ...) by
	// setting LLMAPIBaseURL to that server.
	LLMProvider string `yaml:"llm_provider,omitempty"`

	// LLMAPIBaseURL is the HTTP base URL (no trailing slash) of the LLM
	// endpoint. Default http://127.0.0.1:4000/v1 assumes a local LiteLLM
	// gateway, matching cccli's deployment convention.
	//
	// We use the IPv4 loopback literal rather than "localhost" on purpose:
	// Go's net resolver prefers IPv6 (::1) on Windows, and Docker Desktop's
	// port publishing to [::1] is unreliable behind WSL2's NAT. Using
	// 127.0.0.1 sidesteps that class of "connection refused" without
	// affecting Linux / macOS users.
	LLMAPIBaseURL string `yaml:"llm_api_base_url,omitempty"`

	// LLMAPIKey must come from CLCLI_LLM_API_KEY. yaml:"-" guarantees it
	// never lands on disk through `clcli config init`.
	LLMAPIKey string `yaml:"-"`

	// LLMModel picks the specific model, e.g. "gpt-4o-mini",
	// "claude-3-5-sonnet-20241022", "Pro/deepseek-ai/DeepSeek-V3".
	LLMModel string `yaml:"llm_model,omitempty"`

	// LLMMaxTokens caps the response length. 0 → provider default; cccli
	// sets 4096 as a safe ceiling for JSON-returning prompts.
	LLMMaxTokens int `yaml:"llm_max_tokens,omitempty"`

	// LLMTemperature controls sampling. 0 → provider default (usually ~0.7).
	LLMTemperature float64 `yaml:"llm_temperature,omitempty"`

	// LLMThinking: when false (default), the daemon asks the model to SKIP
	// its reasoning phase by sending `enable_thinking: false` — this makes
	// Qwen3 / DeepSeek-R1 style models respond fast with clean JSON rather
	// than streaming <think>...</think> blocks. Even when enabled, any
	// leaked <think> tags are stripped from the response. Set true only if
	// your prompts benefit from chain-of-thought.
	LLMThinking bool `yaml:"llm_thinking,omitempty"`
}

// userBaseDir returns the per-user base directory (~/.clawlink) that holds
// the per-profile data directories and the legacy flat layout.
func userBaseDir() string {
	home := os.Getenv("HOME")
	if home == "" {
		home = os.Getenv("USERPROFILE")
	}
	return filepath.Join(home, ".clawlink")
}

// DefaultConfig returns a Config with sensible defaults (non-profile-scoped).
// Load() is responsible for layering profile-scoped HomeDir on top.
//
// LLM defaults are populated so `clcli config init` writes visible llm_*
// keys, signalling to new users that daemon mode exists. Base URL points
// at a local LiteLLM gateway by convention (same as cccli): operators run
// one gateway with their real provider keys, every CLI connects to it.
// The API key is intentionally left blank — set CLCLI_LLM_API_KEY so
// secrets never touch disk.
func DefaultConfig() *Config {
	return &Config{
		APIBaseURL:     "https://www.clawlink.net/api/v1",
		HomeDir:        userBaseDir(),
		ChainID:        11111111,
		RPCURL:         "https://evm.clawcoin.com",
		Denom:          "CC",
		GasLimit:       21000,
		GasPrice:       "1000000000", // 1 gwei
		LLMProvider:    "openai",
		LLMAPIBaseURL:  "http://127.0.0.1:4000/v1",
		LLMModel:       "gpt-4o-mini",
		LLMMaxTokens:   4096,
		LLMTemperature: 0.7,
		LLMThinking:    false,
		// LLMAPIKey left empty → set via CLCLI_LLM_API_KEY
	}
}

// Load reads configuration from (in order): defaults → profile scoping →
// YAML file → profile reassertion → env vars.
//
// The profile scopes HomeDir to $HOME/.clawlink/profiles/<profile>/. When
// profile is empty, DefaultProfile is used. Profile is applied BEFORE the
// YAML file is located (so the right clcli.yaml is found) and RE-applied
// after YAML parse (so a stale home_dir value in an old config file cannot
// poison the runtime path). CLCLI_HOME_DIR still wins as the final override.
//
// If cfgFile is empty, Load looks for the profile-scoped clcli.yaml first,
// then ./clcli.yaml as a dev convenience.
func Load(cfgFile, profile string) (*Config, error) {
	if profile == "" {
		profile = DefaultProfile
	}
	if !profileNamePattern.MatchString(profile) {
		return nil, fmt.Errorf("invalid profile name %q (must match [A-Za-z0-9][A-Za-z0-9_-]{0,63})", profile)
	}

	cfg := DefaultConfig()
	baseDir := cfg.HomeDir // ~/.clawlink
	profileDir := filepath.Join(baseDir, "profiles", profile)

	// One-time silent migration for users upgrading from the pre-profile
	// layout. Only runs when the DEFAULT profile is in use and the profile
	// directory has not been created yet; this is the exact condition under
	// which an existing install would otherwise lose its session and keys.
	if profile == DefaultProfile {
		migrateLegacyHome(baseDir, profileDir)
	}

	cfg.HomeDir = profileDir

	// 1. Locate + parse YAML file (optional).
	path := cfgFile
	if path == "" {
		candidates := []string{
			filepath.Join(cfg.HomeDir, "clcli.yaml"),
			"clcli.yaml",
		}
		for _, p := range candidates {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	// 2. Re-assert profile-scoped HomeDir. `yaml:"-"` already prevents a
	// home_dir key from being read, but this guards against any future
	// regressions in the Config struct tags.
	cfg.HomeDir = profileDir

	// 3. Env-var overrides (highest priority — an explicit CLCLI_HOME_DIR
	// still escapes the profile scope, which is the documented behaviour).
	applyEnv(cfg)

	// 4. Ensure home dir exists.
	if err := os.MkdirAll(cfg.HomeDir, 0700); err != nil {
		return nil, fmt.Errorf("create home dir: %w", err)
	}

	return cfg, nil
}

// migrateLegacyHome moves the pre-profile flat layout into the default
// profile directory in one atomic-ish pass. All operations are best-effort:
// if anything fails the user can re-authenticate, which is strictly better
// than failing startup.
func migrateLegacyHome(legacyDir, profileDir string) {
	// If the profile directory already exists, migration has either already
	// happened or the user explicitly created the profile layout.
	if _, err := os.Stat(profileDir); err == nil {
		return
	}

	// Only migrate when at least one legacy artifact is present.
	legacyItems := []string{"clcli.yaml", "session.json", "keystore"}
	found := false
	for _, name := range legacyItems {
		if _, err := os.Stat(filepath.Join(legacyDir, name)); err == nil {
			found = true
			break
		}
	}
	if !found {
		return
	}

	if err := os.MkdirAll(profileDir, 0700); err != nil {
		return
	}
	for _, name := range legacyItems {
		src := filepath.Join(legacyDir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(profileDir, name)
		_ = os.Rename(src, dst) // best-effort
	}
}

// Save writes the config to a YAML file.
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// applyEnv overrides config values from CLCLI_* environment variables.
func applyEnv(c *Config) {
	if v := os.Getenv("CLCLI_API_BASE_URL"); v != "" {
		c.APIBaseURL = v
	}
	if v := os.Getenv("CLCLI_HOME_DIR"); v != "" {
		c.HomeDir = v
	}
	if v := os.Getenv("CLCLI_RPC_URL"); v != "" {
		c.RPCURL = v
	}
	if v := os.Getenv("CLCLI_DENOM"); v != "" {
		c.Denom = v
	}
	if v := os.Getenv("CLCLI_GAS_PRICE"); v != "" {
		c.GasPrice = v
	}
	if v := os.Getenv("CLCLI_CHAIN_ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.ChainID = n
		}
	}
	if v := os.Getenv("CLCLI_GAS_LIMIT"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			c.GasLimit = n
		}
	}

	// ── LLM ────────────────────────────────────────────────────────────────
	// Env var names match cccli (CCCLI_LLM_*) with the clcli prefix.
	if v := os.Getenv("CLCLI_LLM_PROVIDER"); v != "" {
		c.LLMProvider = v
	}
	if v := os.Getenv("CLCLI_LLM_API_KEY"); v != "" {
		c.LLMAPIKey = v
	}
	if v := os.Getenv("CLCLI_LLM_API_BASE_URL"); v != "" {
		c.LLMAPIBaseURL = v
	}
	if v := os.Getenv("CLCLI_LLM_MODEL"); v != "" {
		c.LLMModel = v
	}
	if v := os.Getenv("CLCLI_LLM_MAX_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.LLMMaxTokens = n
		}
	}
	if v := os.Getenv("CLCLI_LLM_TEMPERATURE"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.LLMTemperature = f
		}
	}
	if v := os.Getenv("CLCLI_LLM_THINKING"); v != "" {
		// Accept the usual truthy forms: 1, true, yes, on (case-insensitive).
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			c.LLMThinking = true
		case "0", "false", "no", "off", "":
			c.LLMThinking = false
		}
	}
}
