// Package config loads jev-cli settings from a small JSON file, with
// environment variables taking precedence.
//
// The API key is read from TYPESAFE_API_KEY, from the config file, or from a
// file named with --api-key-file. It is deliberately never accepted as a
// command-line flag: arguments are visible in the process list.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/harlleyoliveira/jev-cli/internal/typesafe"
)

// Environment variables read by Load.
const (
	EnvModel   = "JEV_MODEL"
	EnvBaseURL = "JEV_BASE_URL"
	EnvConfig  = "JEV_CONFIG"
)

// Config holds every tunable.
type Config struct {
	APIKey string `json:"api_key"`
	Model  string `json:"model"`
	// BaseURL is overridable so jev-cli can be pointed at a mock or a gateway.
	BaseURL string `json:"base_url"`
	// MaxEntries caps how many directory entries go into the state.
	MaxEntries int `json:"max_entries"`
	// Thresholds, all optional.
	MinConfidence        float64 `json:"min_confidence"`
	ClarityThreshold     float64 `json:"clarity_threshold"`
	DestructiveThreshold float64 `json:"destructive_threshold"`
	InjectionThreshold   float64 `json:"injection_threshold"`
	SeverityThreshold    float64 `json:"severity_threshold"`
	TimeoutSeconds       int     `json:"timeout_seconds"`
	NoColor              bool    `json:"no_color"`
	AllowWrite           bool    `json:"allow_write"`

	warnings []string
}

// Default returns the starting configuration.
func Default() Config {
	return Config{
		Model:                typesafe.DefaultModel,
		BaseURL:              typesafe.DefaultBaseURL,
		MaxEntries:           120,
		MinConfidence:        0.5,
		ClarityThreshold:     0.35,
		DestructiveThreshold: 0.35,
		InjectionThreshold:   0.70,
		SeverityThreshold:    2.0,
		TimeoutSeconds:       30,
	}
}

// Path returns the config file location: $JEV_CONFIG, else
// $XDG_CONFIG_HOME/jev/config.json, else ~/.config/jev/config.json.
func Path() string {
	if p := os.Getenv(EnvConfig); p != "" {
		return p
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "jev", "config.json")
}

// Load reads the config file if it exists, then applies environment
// overrides. A missing file is not an error.
func Load() (Config, string, error) {
	cfg := Default()
	path := Path()

	var warnings []string
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := json.Unmarshal(data, &cfg); err != nil {
				return cfg, path, fmt.Errorf("%s: %w", path, err)
			}
			if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
				warnings = append(warnings,
					fmt.Sprintf("%s is readable by other users; run chmod 600", path))
			}
		case !errors.Is(err, os.ErrNotExist):
			return cfg, path, err
		}
	}

	if v := strings.TrimSpace(os.Getenv(typesafe.EnvAPIKey)); v != "" {
		cfg.APIKey = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvModel)); v != "" {
		cfg.Model = v
	}
	if v := strings.TrimSpace(os.Getenv(EnvBaseURL)); v != "" {
		cfg.BaseURL = v
	}

	cfg.warnings = warnings
	return cfg, path, nil
}

// warnings travels with the config so the CLI can print it without threading a
// second return value everywhere. Unexported, so it never round-trips to JSON.
// Warnings returns non-fatal problems worth telling the user about.
func (c Config) Warnings() []string { return c.warnings }
