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

func TestLoadRejectsBadTimeouts(t *testing.T) {
	cases := map[string]string{
		"statement_timeout below 1ms": "    statement_timeout: 500us\n",
		"statement_timeout negative":  "    statement_timeout: -1s\n",
		"lock_timeout below 1ms":      "    lock_timeout: 999us\n",
		"lock_timeout negative":       "    lock_timeout: -2s\n",
		"http not above statement":    "    statement_timeout: 30s\n",
		"http below statement":        "    statement_timeout: 20s\n    http_timeout: 10s\n",
		"http negative":               "    http_timeout: -1s\n",
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeCfg(t, full+extra))
			if err == nil || !strings.Contains(err.Error(), "local") || !strings.Contains(err.Error(), "timeout") {
				t.Fatalf("got err %v, want a timeout error naming the environment", err)
			}
		})
	}
}

func TestLoadAcceptsValidTimeouts(t *testing.T) {
	c, err := Load(writeCfg(t, full+"    statement_timeout: 1ms\n    lock_timeout: 1ms\n    http_timeout: 2ms\n"))
	if err != nil {
		t.Fatal(err)
	}
	if e := c.Environments["local"]; e.StatementTimeout != time.Millisecond || e.HTTPTimeout != 2*time.Millisecond {
		t.Fatalf("got %+v", e)
	}
}

func TestAllowedDatabasesDefaultsToDatabase(t *testing.T) {
	c, err := Load(writeCfg(t, full))
	if err != nil {
		t.Fatal(err)
	}
	e := c.Environments["local"]
	if got := e.AllowedDatabases(); len(got) != 1 || got[0] != "glchat" {
		t.Fatalf("AllowedDatabases = %v, want [glchat]", got)
	}
}

func TestAllowedDatabasesKeepsConfigOrder(t *testing.T) {
	c, err := Load(writeCfg(t, full+"    databases: [dev, glchat, dev2]\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := c.Environments["local"].AllowedDatabases()
	want := []string{"dev", "glchat", "dev2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("AllowedDatabases = %v, want %v", got, want)
	}
	// The caller must not be able to change the allowlist through the result.
	got[0] = "evil"
	if c.Environments["local"].AllowedDatabases()[0] != "dev" {
		t.Fatal("AllowedDatabases returned the internal slice")
	}
}

func TestLoadRejectsBadDatabases(t *testing.T) {
	cases := map[string]struct{ extra, want string }{
		"empty entry":      {"    databases: [glchat, \"\"]\n", "empty"},
		"duplicate entry":  {"    databases: [glchat, dev, dev]\n", "dev"},
		"default missing":  {"    databases: [dev, dev2]\n", "glchat"},
		"empty list":       {"    databases: []\n", "databases"},
		"whitespace entry": {"    databases: [glchat, \" \"]\n", "empty"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeCfg(t, full+tc.extra))
			if err == nil || !strings.Contains(err.Error(), `"local"`) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got err %v, want an error naming the environment and %q", err, tc.want)
			}
		})
	}
}

func TestResolveDatabase(t *testing.T) {
	c, err := Load(writeCfg(t, full+"    databases: [glchat, dev, dev2]\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := c.Environments["local"]
	for in, want := range map[string]string{"": "glchat", "glchat": "glchat", "dev": "dev", "dev2": "dev2"} {
		if got, err := e.ResolveDatabase(in); err != nil || got != want {
			t.Errorf("ResolveDatabase(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"dev3", "DEV", " dev", "postgres"} {
		_, err := e.ResolveDatabase(in)
		want := `database "` + in + `" is not allowed for environment "local"; allowed: glchat, dev, dev2`
		if err == nil || err.Error() != want {
			t.Errorf("ResolveDatabase(%q) err = %v, want %q", in, err, want)
		}
	}
}

func TestResolveDatabaseWithoutList(t *testing.T) {
	e := &Env{Name: "x", Database: "app"}
	if got, err := e.ResolveDatabase(""); err != nil || got != "app" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := e.ResolveDatabase("other"); err == nil || !strings.Contains(err.Error(), "allowed: app") {
		t.Fatalf("got %v", err)
	}
}
