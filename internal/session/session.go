// Package session manages local storage of JWT tokens and Agent API keys.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Session is the locally persisted session state.
type Session struct {
	JWT     string `json:"jwt,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
	UserID  string `json:"user_id,omitempty"`
	Email   string `json:"email,omitempty"`
	IsAgent bool   `json:"is_agent,omitempty"`
}

// Path returns the session file path inside homeDir.
func Path(homeDir string) string {
	return filepath.Join(homeDir, "session.json")
}

// Load reads the session file (empty Session if missing).
func Load(homeDir string) (*Session, error) {
	path := Path(homeDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Session{}, nil
		}
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse session: %w", err)
	}
	return &s, nil
}

// Save writes the session to disk with 0600 permissions.
func Save(homeDir string, s *Session) error {
	path := Path(homeDir)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// Clear removes the session file (logout).
func Clear(homeDir string) error {
	path := Path(homeDir)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
