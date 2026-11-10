package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Homeserver      string        `yaml:"homeserver"`
	AccessToken     string        `yaml:"access_token"`
	AccessTokenFile string        `yaml:"access_token_file"`
	Rooms           []string      `yaml:"rooms"`
	GitHubToken     string        `yaml:"github_token"`
	Summary         SummaryConfig `yaml:"summary"`
}

type SummaryConfig struct {
	Enabled           bool   `yaml:"enabled"`
	Time              string `yaml:"time"`                 // HH:MM, default 23:59
	Timezone          string `yaml:"timezone"`             // IANA name, default UTC
	StateFile         string `yaml:"state_file"`           // empty: in memory only
	MaxCommitsPerRepo int    `yaml:"max_commits_per_repo"` // default 50, -1 for no limit
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Homeserver == "" {
		return nil, errors.New("homeserver is required")
	}
	if cfg.AccessToken == "" && cfg.AccessTokenFile != "" {
		tokenPath := cfg.AccessTokenFile
		if !filepath.IsAbs(tokenPath) {
			tokenPath = filepath.Join(filepath.Dir(path), tokenPath)
		}
		tok, err := os.ReadFile(tokenPath)
		if err != nil {
			return nil, fmt.Errorf("read access_token_file: %w", err)
		}
		cfg.AccessToken = strings.TrimSpace(string(tok))
	}
	if cfg.AccessToken == "" {
		return nil, errors.New("access_token (or access_token_file) is required")
	}
	if cfg.GitHubToken == "" {
		cfg.GitHubToken = os.Getenv("GITHUB_TOKEN")
	}
	if cfg.Summary.Time == "" {
		cfg.Summary.Time = "23:59"
	}
	if cfg.Summary.Timezone == "" {
		cfg.Summary.Timezone = "UTC"
	}
	if cfg.Summary.MaxCommitsPerRepo == 0 {
		cfg.Summary.MaxCommitsPerRepo = 50
	}
	if p := cfg.Summary.StateFile; p != "" && !filepath.IsAbs(p) {
		cfg.Summary.StateFile = filepath.Join(filepath.Dir(path), p)
	}
	if len(cfg.Rooms) == 0 {
		return nil, errors.New("at least one room is required")
	}
	return &cfg, nil
}
