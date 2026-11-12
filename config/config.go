// Package config loads adminer-bridge environment definitions.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// Env describes one Adminer-backed Postgres environment.
type Env struct {
	Name             string        // map key
	URL              string        `yaml:"url"`
	Server           string        `yaml:"server"`
	Username         string        `yaml:"username"`
	Database         string        `yaml:"database"`
	Schema           string        `yaml:"schema"`
	PasswordEnv      string        `yaml:"password_env"`
	StatementTimeout time.Duration `yaml:"statement_timeout"`
	LockTimeout      time.Duration `yaml:"lock_timeout"`
	HTTPTimeout      time.Duration `yaml:"http_timeout"`
}

// Config is the top-level config file.
type Config struct {
	Environments map[string]*Env `yaml:"environments"`
}

// Load parses the file at path, applies defaults and validates it.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if len(c.Environments) == 0 {
		return nil, fmt.Errorf("config %s: no environments defined", path)
	}
	for name, e := range c.Environments {
		if e == nil {
			return nil, fmt.Errorf("environment %q: empty definition", name)
		}
		e.Name = name
		for _, r := range []struct{ field, val string }{
			{"url", e.URL}, {"server", e.Server}, {"username", e.Username},
			{"database", e.Database}, {"password_env", e.PasswordEnv},
		} {
			if r.val == "" {
				return nil, fmt.Errorf("environment %q: missing required field %q", name, r.field)
			}
		}
		if e.Schema == "" {
			e.Schema = "public"
		}
		if e.StatementTimeout == 0 {
			e.StatementTimeout = 15 * time.Second
		}
		if e.LockTimeout == 0 {
			e.LockTimeout = 2 * time.Second
		}
		if e.HTTPTimeout == 0 {
			e.HTTPTimeout = 30 * time.Second
		}
	}
	return &c, nil
}

// DefaultPath returns ADMINER_BRIDGE_CONFIG or ~/.config/adminer-bridge/config.yaml.
func DefaultPath() string {
	if p := os.Getenv("ADMINER_BRIDGE_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "adminer-bridge", "config.yaml")
	}
	return filepath.Join(home, ".config", "adminer-bridge", "config.yaml")
}

// Password reads the password from the environment variable named in config.
func (e *Env) Password() (string, error) {
	if v := os.Getenv(e.PasswordEnv); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("environment variable %s is not set or empty", e.PasswordEnv)
}

// Names returns the environment names, sorted.
func (c *Config) Names() []string {
	n := make([]string, 0, len(c.Environments))
	for k := range c.Environments {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}
