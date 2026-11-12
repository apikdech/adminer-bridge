// Package integration holds opt-in tests that run against a real Adminer.
//
// Run: ADMINER_BRIDGE_IT=1 ADMINER_LOCAL_PASSWORD=... go test ./integration/ -v -count=1
//
// All tests share one bridge (one Adminer login per environment) because
// Adminer throttles IPs after failed logins.
package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"adminer-bridge/adminer"
	"adminer-bridge/bridge"
	"adminer-bridge/config"
	"adminer-bridge/gate"
)

const (
	container = "glchat-be-database-1"
	envLocal  = "local"
	envStmt   = "local_stmt1s"
)

const testConfig = `environments:
  local:
    url: http://localhost:8080/
    server: database
    username: postgres
    database: glchat
    schema: public
    password_env: ADMINER_LOCAL_PASSWORD
  local_stmt1s:
    url: http://localhost:8080/
    server: database
    username: postgres
    database: glchat
    schema: public
    password_env: ADMINER_LOCAL_PASSWORD
    statement_timeout: 1s
`

var (
	once   sync.Once
	shared *bridge.Bridge
	cfg    *config.Config
	setErr error
	tmpDir string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if tmpDir != "" {
		os.RemoveAll(tmpDir)
	}
	os.Exit(code)
}

// setup skips the test unless integration tests are enabled, then returns
// the shared bridge.
func setup(t *testing.T) *bridge.Bridge {
	t.Helper()
	if os.Getenv("ADMINER_BRIDGE_IT") != "1" {
		t.Skip("set ADMINER_BRIDGE_IT=1 to run integration tests")
	}
	if os.Getenv("ADMINER_LOCAL_PASSWORD") == "" {
		t.Skip("ADMINER_LOCAL_PASSWORD is not set")
	}
	once.Do(func() {
		tmpDir, setErr = os.MkdirTemp("", "adminer-bridge-it")
		if setErr != nil {
			return
		}
		path := filepath.Join(tmpDir, "config.yaml")
		if setErr = os.WriteFile(path, []byte(testConfig), 0o600); setErr != nil {
			return
		}
		if cfg, setErr = config.Load(path); setErr != nil {
			return
		}
		shared, setErr = bridge.New(cfg)
	})
	if setErr != nil {
		t.Fatalf("setup: %v", setErr)
	}
	return shared
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestITSelect(t *testing.T) {
	b := setup(t)
	res, err := b.Query(ctx(t), envLocal, "", "select 1 as a, null as b", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 2 {
		t.Fatalf("rows = %v", res.Rows)
	}
	if got := res.Rows[0][0]; got == nil || *got != "1" {
		t.Errorf("a = %v, want \"1\"", got)
	}
	if res.Rows[0][1] != nil {
		t.Errorf("b = %q, want NULL", *res.Rows[0][1])
	}
}

func TestITFunctionsLoaded(t *testing.T) {
	b := setup(t)
	if _, err := b.Query(ctx(t), envLocal, "", "select count(*) from pg_catalog.pg_class", 0); err != nil {
		t.Errorf("count(*) from pg_class: %v", err)
	}
	if _, err := b.Query(ctx(t), envLocal, "", "select pg_sleep(0)", 0); err == nil {
		t.Error("pg_sleep(0) was accepted")
	}
}

// TestITLayer3 bypasses layers 1 and 2 on purpose, with a raw client, to show
// that Postgres itself refuses writes inside the wrapper transaction.
func TestITLayer3(t *testing.T) {
	b := setup(t)
	client, err := adminer.NewClient(cfg.Environments[envLocal])
	if err != nil {
		t.Fatal(err)
	}
	batch := gate.Wrap("CREATE TABLE zz_it_probe(i int)", gate.Timeouts{Statement: 15 * time.Second, Lock: 2 * time.Second})
	blocks, err := client.Exec(ctx(t), cfg.Environments[envLocal].Database, batch, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) <= gate.WrappedStmtIndex {
		t.Fatalf("got %d blocks, want more than %d", len(blocks), gate.WrappedStmtIndex)
	}
	blk := blocks[gate.WrappedStmtIndex]
	if blk.Kind != adminer.KindError || !strings.Contains(blk.Message, "read-only transaction") {
		t.Fatalf("block %d = kind %v, message %q; want error containing \"read-only transaction\"",
			gate.WrappedStmtIndex, blk.Kind, blk.Message)
	}
	res, err := b.Query(ctx(t), envLocal, "", "select to_regclass('zz_it_probe')", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.Rows[0][0] != nil {
		t.Errorf("zz_it_probe exists or result is odd: %v", res.Rows)
	}
}

func psql(t *testing.T, sql string) *exec.Cmd {
	t.Helper()
	return exec.Command("docker", "exec", container, "psql", "-U", "postgres", "-d", "glchat", "-v", "ON_ERROR_STOP=1", "-c", sql)
}

func TestITLockTimeout(t *testing.T) {
	b := setup(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	const table = "zz_it_lock_probe"
	if out, err := psql(t, "CREATE TABLE "+table+"(i int)").CombinedOutput(); err != nil {
		t.Skipf("cannot create scratch table via docker exec: %v: %s", err, out)
	}
	t.Cleanup(func() {
		if out, err := psql(t, "DROP TABLE IF EXISTS "+table).CombinedOutput(); err != nil {
			t.Errorf("drop %s: %v: %s", table, err, out)
		}
	})

	holder := psql(t, "BEGIN; LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(5); COMMIT;")
	if err := holder.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	done := make(chan struct{})
	go func() { holder.Wait(); close(done) }()
	t.Cleanup(func() { <-done })
	time.Sleep(1 * time.Second) // let the holder take the lock

	start := time.Now()
	_, err := b.Query(ctx(t), envLocal, "", "select * from "+table, 0)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("query succeeded while the table was locked")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "lock timeout") {
		t.Errorf("error does not mention lock timeout: %v", err)
	}
	if elapsed > 4*time.Second {
		t.Errorf("query took %v, want about the 2s lock_timeout", elapsed)
	}
}

func TestITStatementTimeout(t *testing.T) {
	b := setup(t)
	_, err := b.Query(ctx(t), envStmt, "", "select count(*) from generate_series(1, 300000000)", 0)
	if err == nil {
		t.Fatal("long query succeeded")
	}
	if !strings.Contains(err.Error(), "statement timeout") {
		t.Errorf("error does not mention statement timeout: %v", err)
	}
}

func TestITRoleWarnings(t *testing.T) {
	b := setup(t)
	// The role check runs on first connect, so make sure we are connected.
	if _, err := b.Query(ctx(t), envLocal, "", "select 1", 0); err != nil {
		t.Fatal(err)
	}
	for _, e := range b.ListEnvironments() {
		if e.Name != envLocal {
			continue
		}
		for _, w := range e.Databases[0].Warnings {
			if strings.Contains(w, "superuser") {
				return
			}
		}
		t.Fatalf("no superuser warning for %s: %v", envLocal, e.Databases[0].Warnings)
	}
	t.Fatalf("environment %s not listed", envLocal)
}

func TestITDescribeTable(t *testing.T) {
	b := setup(t)
	tables, err := b.ListTables(ctx(t), envLocal, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tables.Rows) == 0 || tables.Rows[0][0] == nil {
		t.Skip("no tables in schema public")
	}
	name := *tables.Rows[0][0]
	desc, err := b.DescribeTable(ctx(t), envLocal, "", name)
	if err != nil {
		t.Fatal(err)
	}
	if desc.Columns == nil || len(desc.Columns.Rows) == 0 {
		t.Errorf("describe %s returned no columns", name)
	}
}
