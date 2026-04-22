// Package config provides minimal YAML + env-var configuration management.
//
// We intentionally avoid viper/cobra-viper to keep the binary small. Loading
// semantics: defaults → YAML file → CLCLI_* env vars (highest priority).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Config holds all configuration for clcli.
type Config struct {
	// ClawLink REST API base (no trailing slash). Public default uses the main
	// production API; local development can override this via config or env.
	APIBaseURL string `yaml:"api_base_url"`

	// Local data directory (stores keystore, session, etc.). Default: ~/.clawlink/
	HomeDir string `yaml:"home_dir"`

	// ClawCoin mainnet/public config by default. Testnet can be selected by
	// overriding chain_id and rpc_url in config or env.
	ChainID int64  `yaml:"chain_id"` // 11111111
	RPCURL  string `yaml:"rpc_url"`  // https://evm.clawcoin.com
	Denom   string `yaml:"denom"`    // CC (display); wei on-chain

	// Gas defaults.
	GasLimit uint64 `yaml:"gas_limit"`
	GasPrice string `yaml:"gas_price"` // wei, decimal
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	home := os.Getenv("HOME")
	if home == "" {
		home = os.Getenv("USERPROFILE")
	}
	return &Config{
		APIBaseURL: "https://www.clawlink.net/api/v1",
		HomeDir:    filepath.Join(home, ".clawlink"),
		ChainID:    11111111,
		RPCURL:     "https://evm.clawcoin.com",
		Denom:      "CC",
		GasLimit:   21000,
		GasPrice:   "1000000000", // 1 gwei
	}
}

// Load reads configuration from (in order): defaults → YAML file → env vars.
// If cfgFile is empty, looks for $HOME/.clawlink/clcli.yaml then ./clcli.yaml.
func Load(cfgFile string) (*Config, error) {
	cfg := DefaultConfig()

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

	// 2. Env-var overrides (highest priority).
	applyEnv(cfg)

	// 3. Ensure home dir exists.
	if err := os.MkdirAll(cfg.HomeDir, 0700); err != nil {
		return nil, fmt.Errorf("create home dir: %w", err)
	}

	return cfg, nil
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
}
