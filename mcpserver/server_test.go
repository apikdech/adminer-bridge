package mcpserver

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"adminer-bridge/adminer"
	"adminer-bridge/bridge"
	"adminer-bridge/config"
)

// fakeExec answers the bridge's init batches and echoes every other
// statement back as a one-row table.
type fakeExec struct{}

func (fakeExec) Exec(_ context.Context, batch string, _ int) ([]adminer.Block, error) {
	parts := strings.Split(batch, ";\n")
	stmt := parts[4]
	blocks := make([]adminer.Block, 0, 6)
	for i := 0; i < 4; i++ {
		blocks = append(blocks, adminer.Block{SQL: parts[i], Kind: adminer.KindMessage, Message: "ok"})
	}
	val := func(s string) [][]*string { return [][]*string{{&s}} }
	main := adminer.Block{SQL: stmt, Kind: adminer.KindTable, Columns: []string{"x"}, Rows: val("1")}
	switch {
	case strings.Contains(stmt, "pg_proc"):
		main.Rows = val("count")
	case strings.Contains(stmt, "rolsuper"):
		main.Rows = val("f")
	case strings.Contains(stmt, "pg_extension"), strings.Contains(stmt, "pg_language"):
		main.Rows = nil
	case strings.Contains(stmt, "null_vs_text"):
		// An SQL NULL cell and a text cell that reads "NULL".
		s := "NULL"
		main.Columns = []string{"a", "null_vs_text"}
		main.Rows = [][]*string{{nil, &s}}
	}
	blocks = append(blocks, main, adminer.Block{SQL: "ROLLBACK", Kind: adminer.KindMessage, Message: "ok"})
	return blocks, nil
}

func connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	cfg := &config.Config{Environments: map[string]*config.Env{"dev": {
		Name: "dev", URL: "http://x/", Database: "db", Schema: "public",
		StatementTimeout: 15 * time.Second, LockTimeout: 2 * time.Second,
	}}}
	b := bridge.NewWithExecutors(cfg, map[string]bridge.Executor{"dev": fakeExec{}})
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	srv := New(b, "test")
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestListsFourTools(t *testing.T) {
	cs := connect(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	want := []string{"describe_table", "list_environments", "list_tables", "query"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
}

func TestQueryToolReturnsResult(t *testing.T) {
	cs := connect(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "query", Arguments: map[string]any{"env": "dev", "sql": "select 1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %+v", res.Content)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var got struct {
		Columns     []string    `json:"columns"`
		Rows        [][]*string `json:"rows"`
		ExecutedSQL string      `json:"executed_sql"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Columns) != 1 || len(got.Rows) != 1 || got.ExecutedSQL == "" {
		t.Fatalf("structured content = %s", raw)
	}
}

func TestQueryToolGateRejection(t *testing.T) {
	cs := connect(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "query", Arguments: map[string]any{"env": "dev", "sql": "delete from users"},
	})
	if err != nil {
		t.Fatalf("want tool error, got protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("IsError = false")
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "rejected by read-only gate [statement_type]") {
		t.Fatalf("text = %q", text)
	}
}

func TestListEnvironmentsAndCatalogTools(t *testing.T) {
	cs := connect(t)
	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_environments", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("list_environments: %v %+v", err, res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(raw), `"environments"`) || !strings.Contains(string(raw), `"dev"`) {
		t.Fatalf("got %s", raw)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_tables", Arguments: map[string]any{"env": "dev"}})
	if err != nil || res.IsError {
		t.Fatalf("list_tables: %v %+v", err, res)
	}
}

func TestQueryToolDistinguishesNullFromNULLText(t *testing.T) {
	cs := connect(t)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "query", Arguments: map[string]any{"env": "dev", "sql": "select null as a, 'NULL' as null_vs_text"},
	})
	if err != nil || res.IsError {
		t.Fatalf("query: %v %+v", err, res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var got struct {
		Rows []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 1 || string(got.Rows[0]) != `[null,"NULL"]` {
		t.Fatalf("rows = %s, want [[null,\"NULL\"]] (structured content %s)", got.Rows, raw)
	}
}
