package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const full = `environments:
  local:
    url: http://localhost:8080/
    server: database
    username: postgres
    database: glchat
    password_env: X_PW
`

func TestLoadAppliesDefaults(t *testing.T) {
	c, err := Load(writeCfg(t, full))
	if err != nil {
		t.Fatal(err)
	}
	e := c.Environments["local"]
	if e.Name != "local" || e.Schema != "public" ||
		e.StatementTimeout != 15*time.Second ||
		e.LockTimeout != 2*time.Second ||
		e.HTTPTimeout != 30*time.Second {
		t.Fatalf("bad defaults: %+v", e)
	}
}

func TestLoadRejectsMissingFields(t *testing.T) {
	for _, f := range []string{"url", "server", "username", "database", "password_env"} {
		var lines []string
		for _, l := range strings.Split(full, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), f+":") {
				continue
			}
			lines = append(lines, l)
		}
		_, err := Load(writeCfg(t, strings.Join(lines, "\n")))
		if err == nil || !strings.Contains(err.Error(), "local") || !strings.Contains(err.Error(), f) {
			t.Errorf("field %s: got err %v", f, err)
		}
	}
}

func TestLoadRejectsInlinePassword(t *testing.T) {
	_, err := Load(writeCfg(t, full+"    password: hunter2\n"))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatal("error leaks password")
	}
}

func TestPasswordFromEnv(t *testing.T) {
	e := &Env{PasswordEnv: "X_PW"}
	t.Setenv("X_PW", "s3cret")
	if p, err := e.Password(); err != nil || p != "s3cret" {
		t.Fatalf("got %q, %v", p, err)
	}
	t.Setenv("X_PW", "")
	_, err := e.Password()
	if err == nil || !strings.Contains(err.Error(), "X_PW") {
		t.Fatalf("got %v", err)
	}
}

func TestDefaultPathEnvOverride(t *testing.T) {
	t.Setenv("ADMINER_BRIDGE_CONFIG", "/x.yaml")
	if got := DefaultPath(); got != "/x.yaml" {
		t.Fatal(got)
	}
}

func TestNamesSorted(t *testing.T) {
	c := &Config{Environments: map[string]*Env{"b": {}, "a": {}}}
	n := c.Names()
	if len(n) != 2 || n[0] != "a" || n[1] != "b" {
		t.Fatal(n)
	}
}
