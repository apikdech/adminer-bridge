package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The URL is unroutable on purpose: these tests must fail before any request.
const cliConfig = `environments:
  dev:
    url: http://127.0.0.1:1/
    server: database
    username: postgres
    database: app
    databases: [app, app2]
    password_env: ADMINER_BRIDGE_CLI_TEST_PW
`

func writeConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(cliConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADMINER_BRIDGE_CLI_TEST_PW", "x")
	return p
}

func TestQueryDBFlagReachesBridge(t *testing.T) {
	cfg := writeConfig(t)
	var out, errOut bytes.Buffer
	// Flags on both sides of the SQL argument, like other flags.
	err := run([]string{"query", "--config", cfg, "--env", "dev", "select 1", "--db", "other"}, &out, &errOut)
	want := `database "other" is not allowed for environment "dev"; allowed: app, app2`
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestUsageMentionsDBFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	_ = run([]string{"help"}, &out, &errOut)
	if !strings.Contains(errOut.String(), "[--db NAME]") {
		t.Fatalf("usage = %q", errOut.String())
	}
}

func TestServeLoadsSecretsBeforeConfig(t *testing.T) {
	cfgPath := writeConfig(t)
	t.Setenv("ADMINER_BRIDGE_CLI_TEST_PW", "")
	sp := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(sp, []byte("ADMINER_BRIDGE_CLI_TEST_PW=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(cfgPath, sp)
	if err != nil {
		t.Fatal(err)
	}
	pw, err := cfg.Environments["dev"].Password()
	if err != nil || pw != "from-file" {
		t.Fatalf("password = %q, %v; want the secrets file value", pw, err)
	}
}

func TestLoadConfigRejectsOpenSecretsFile(t *testing.T) {
	cfgPath := writeConfig(t)
	sp := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(sp, []byte("A=b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sp, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(cfgPath, sp); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("err = %v, want a chmod 600 error", err)
	}
}
