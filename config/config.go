// Package config loads adminer-bridge environment definitions.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Env describes one Adminer-backed Postgres environment.
type Env struct {
	Name             string        // map key
	URL              string        `yaml:"url"`
	Server           string        `yaml:"server"`
	Username         string        `yaml:"username"`
	Database         string        `yaml:"database"`  // default database
	Databases        []string      `yaml:"databases"` // optional allowlist; must contain Database
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
		if err := e.checkDatabases(); err != nil {
			return nil, err
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
		// The wrapper renders timeouts in whole milliseconds, and '0ms'
		// means no limit in Postgres.
		for _, d := range []struct {
			field string
			val   time.Duration
		}{{"statement_timeout", e.StatementTimeout}, {"lock_timeout", e.LockTimeout}} {
			if d.val < time.Millisecond {
				return nil, fmt.Errorf("environment %q: %s must be at least 1ms, got %s", name, d.field, d.val)
			}
		}
		if e.HTTPTimeout <= e.StatementTimeout {
			return nil, fmt.Errorf("environment %q: http_timeout (%s) must be greater than statement_timeout (%s)",
				name, e.HTTPTimeout, e.StatementTimeout)
		}
	}
	return &c, nil
}

// checkDatabases validates the optional databases allowlist.
func (e *Env) checkDatabases() error {
	if e.Databases == nil {
		return nil
	}
	if len(e.Databases) == 0 {
		return fmt.Errorf("environment %q: databases is empty; remove it or list the allowed databases", e.Name)
	}
	seen := map[string]bool{}
	for i, d := range e.Databases {
		if strings.TrimSpace(d) == "" {
			return fmt.Errorf("environment %q: databases entry %d is empty", e.Name, i+1)
		}
		if seen[d] {
			return fmt.Errorf("environment %q: databases lists %q more than once", e.Name, d)
		}
		seen[d] = true
	}
	if !seen[e.Database] {
		return fmt.Errorf("environment %q: databases must include the default database %q", e.Name, e.Database)
	}
	return nil
}

// AllowedDatabases returns the databases callers may use in this
// environment, in config order. Without a databases list it is [Database].
// The result is a copy.
func (e *Env) AllowedDatabases() []string {
	if len(e.Databases) == 0 {
		return []string{e.Database}
	}
	return append([]string(nil), e.Databases...)
}

// ResolveDatabase maps a caller's database choice to the database to use:
// "" is the default database, an allowed name (exact, case-sensitive match)
// is itself, anything else is an error that lists the allowed databases.
func (e *Env) ResolveDatabase(name string) (string, error) {
	if name == "" {
		return e.Database, nil
	}
	allowed := e.AllowedDatabases()
	for _, d := range allowed {
		if d == name {
			return name, nil
		}
	}
	return "", fmt.Errorf("database %q is not allowed for environment %q; allowed: %s",
		name, e.Name, strings.Join(allowed, ", "))
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
