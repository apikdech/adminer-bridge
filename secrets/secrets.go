// Package secrets loads KEY=VALUE secrets from a file into the process
// environment. It is stdlib only so it can be copied between repos.
//
// No secret value is ever included in an error or log line: errors name the
// file path and line number only.
package secrets

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

var keyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type entry struct {
	key   string
	value string
}

// Load reads KEY=VALUE lines from the file at path and sets each variable
// with os.Setenv, unless the variable is already set to a non-empty value.
//
// Blank lines and lines starting with # are skipped. A leading "export " is
// allowed, and one pair of matching surrounding single or double quotes is
// removed from the value. The file must not be readable or writable by group
// or others (chmod 600). The whole file is parsed before any variable is set,
// so a bad file sets nothing.
func Load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	defer f.Close()

	// Check the mode on the open file so the checked file is the one read.
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("secrets: %s is readable by group or others; run: chmod 600 %s", path, path)
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}

	entries, err := parse(path, string(data))
	if err != nil {
		return err
	}

	for _, e := range entries {
		if os.Getenv(e.key) != "" {
			continue
		}
		if err := os.Setenv(e.key, e.value); err != nil {
			return fmt.Errorf("secrets: set %s from %s: %w", e.key, path, err)
		}
	}
	return nil
}

func parse(path, content string) ([]entry, error) {
	var entries []entry
	for i, line := range strings.Split(content, "\n") {
		lineNo := i + 1
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "export "); ok {
			line = strings.TrimSpace(rest)
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("secrets: %s line %d: not a KEY=VALUE pair", path, lineNo)
		}
		key = strings.TrimSpace(key)
		if !keyPattern.MatchString(key) {
			return nil, fmt.Errorf("secrets: %s line %d: invalid key name", path, lineNo)
		}

		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			first, last := value[0], value[len(value)-1]
			if (first == '"' || first == '\'') && first == last {
				value = value[1 : len(value)-1]
			}
		}
		entries = append(entries, entry{key: key, value: value})
	}
	return entries, nil
}
