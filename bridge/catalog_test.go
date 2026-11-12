package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"adminer-bridge/adminer"
	"adminer-bridge/config"
	"adminer-bridge/gate"
)

func catalogBridge(f *fake) *Bridge {
	cfg := &config.Config{Environments: map[string]*config.Env{"local": {
		Name: "local", URL: "http://x/", Database: "db", Schema: "public",
		StatementTimeout: 15 * time.Second, LockTimeout: 2 * time.Second,
	}}}
	return NewWithExecutors(cfg, map[string]Executor{"local": f})
}

func catalogFake() *fake {
	return &fake{procs: []string{"pg_get_constraintdef"}, super: "f"}
}

// catalogStmts returns the statements of non-init calls.
func catalogStmts(f *fake) []string {
	var out []string
	for _, c := range f.calls[4:] {
		out = append(out, stmtOf(c.batch))
	}
	return out
}

func TestListTablesUsesEnvSchema(t *testing.T) {
	f := catalogFake()
	b := catalogBridge(f)
	if _, err := b.ListTables(context.Background(), "local", "", ""); err != nil {
		t.Fatal(err)
	}
	s := catalogStmts(f)
	if len(s) != 1 || !strings.Contains(s[0], "'public'") || !strings.Contains(s[0], "information_schema.tables") {
		t.Fatalf("unexpected statements: %v", s)
	}
	if f.calls[4].limit != 0 {
		t.Errorf("limit = %d, want 0", f.calls[4].limit)
	}
}

func TestDescribeTableQuotesName(t *testing.T) {
	f := catalogFake()
	b := catalogBridge(f)
	_, err := b.DescribeTable(context.Background(), "local", "", "users'; drop")
	var rej *gate.RejectError
	if !errors.As(err, &rej) || rej.Rule != gate.RuleUnsafeChars {
		t.Fatalf("want unsafe_characters RejectError, got %v", err)
	}
	if len(f.calls) != 4 {
		t.Fatalf("want only init batches, got %d", len(f.calls))
	}

	f = catalogFake()
	f.user = func(stmt string, _ int) []adminer.Block {
		return tableBlocks(stmt, []string{"column_name"}, col([]string{"id"}))
	}
	b = catalogBridge(f)
	if _, err := b.DescribeTable(context.Background(), "local", "", "o'brien"); err != nil {
		t.Fatal(err)
	}
	if s := catalogStmts(f); len(s) == 0 || !strings.Contains(s[0], "'o''brien'") {
		t.Fatalf("unexpected statements: %v", s)
	}
}

func TestDescribeTableSchemaQualified(t *testing.T) {
	f := catalogFake()
	f.user = func(stmt string, _ int) []adminer.Block {
		return tableBlocks(stmt, []string{"column_name"}, col([]string{"id"}))
	}
	b := catalogBridge(f)
	d, err := b.DescribeTable(context.Background(), "local", "", "auth.users")
	if err != nil {
		t.Fatal(err)
	}
	if d.Schema != "auth" || d.Table != "users" {
		t.Errorf("got %s.%s", d.Schema, d.Table)
	}
	s := catalogStmts(f)
	if len(s) != 3 {
		t.Fatalf("want 3 statements, got %v", s)
	}
	for _, st := range s {
		if !strings.Contains(st, "'auth'") || !strings.Contains(st, "'users'") {
			t.Errorf("statement missing schema/table filter: %s", st)
		}
	}
	if !strings.Contains(s[2], "pg_get_constraintdef") {
		t.Errorf("third statement should be foreign keys: %s", s[2])
	}
}

func TestDescribeTableNotFound(t *testing.T) {
	f := catalogFake()
	f.user = func(stmt string, _ int) []adminer.Block {
		return tableBlocks(stmt, []string{"column_name"}, nil)
	}
	b := catalogBridge(f)
	_, err := b.DescribeTable(context.Background(), "local", "", "nope")
	if err == nil || !strings.Contains(err.Error(), `table public.nope not found in environment "local", database "db"`) {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestCatalogUsesRequestedDatabase(t *testing.T) {
	f := catalogFake()
	f.user = func(stmt string, _ int) []adminer.Block {
		return tableBlocks(stmt, []string{"column_name"}, col([]string{"id"}))
	}
	cfg := &config.Config{Environments: map[string]*config.Env{"local": {
		Name: "local", URL: "http://x/", Database: "db", Databases: []string{"db", "other"}, Schema: "public",
		StatementTimeout: 15 * time.Second, LockTimeout: 2 * time.Second,
	}}}
	b := NewWithExecutors(cfg, map[string]Executor{"local": f})
	ctx := context.Background()
	lt, err := b.ListTables(ctx, "local", "other", "")
	if err != nil {
		t.Fatal(err)
	}
	if lt.Database != "other" {
		t.Errorf("ListTables database = %q, want other", lt.Database)
	}
	d, err := b.DescribeTable(ctx, "local", "other", "users")
	if err != nil {
		t.Fatal(err)
	}
	if d.Database != "other" {
		t.Errorf("DescribeTable database = %q, want other", d.Database)
	}
	for _, c := range f.calls {
		if c.db != "other" {
			t.Errorf("batch sent to %q, want other", c.db)
		}
	}
	d, err = b.DescribeTable(ctx, "local", "", "users")
	if err != nil {
		t.Fatal(err)
	}
	if d.Database != "db" {
		t.Errorf("default DescribeTable database = %q, want db", d.Database)
	}
}
