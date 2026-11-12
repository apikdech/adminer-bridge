package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"adminer-bridge/adminer"
	"adminer-bridge/bridge"
	"adminer-bridge/config"
	"adminer-bridge/mcpserver"
)

const testToken = "0123456789abcdef0123456789abcdef" // 32 characters

// fakeExec answers the bridge's init batches and echoes every other
// statement as a one-row table (same pattern as the mcpserver tests).
type fakeExec struct{}

func (fakeExec) Exec(_ context.Context, _, batch string, _ int) ([]adminer.Block, error) {
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
		main.Rows = append(append(val("count"), val("current_database")...), val("has_database_privilege")...)
	case strings.Contains(stmt, "pg_database"):
		_, name, _ := strings.Cut(stmt, "datname = '")
		name, _, _ = strings.Cut(name, "'")
		main.Columns = []string{"datname"}
		main.Rows = val(name)
	case strings.Contains(stmt, "rolsuper"):
		main.Rows = val("f")
	case strings.Contains(stmt, "pg_extension"), strings.Contains(stmt, "pg_language"):
		main.Rows = nil
	}
	blocks = append(blocks, main, adminer.Block{SQL: "ROLLBACK", Kind: adminer.KindMessage, Message: "ok"})
	return blocks, nil
}

// newTestServer serves the HTTP handler over a fake bridge and returns
// the server and the log output.
func newTestServer(t *testing.T) (*httptest.Server, *syncBuffer) {
	t.Helper()
	cfg := &config.Config{Environments: map[string]*config.Env{"dev": {
		Name: "dev", URL: "http://x/", Database: "db", Databases: []string{"db"}, Schema: "public",
		StatementTimeout: 15 * time.Second, LockTimeout: 2 * time.Second,
	}}}
	b := bridge.NewWithExecutors(cfg, map[string]bridge.Executor{"dev": fakeExec{}})
	logs := &syncBuffer{}
	h := newHTTPHandler(mcpserver.New(b, "test"), testToken, log.New(logs, "", 0))
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, logs
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.next.RoundTrip(r)
}

func connectHTTP(ctx context.Context, url, token string) (*mcp.ClientSession, error) {
	tr := &mcp.StreamableClientTransport{
		Endpoint:   url + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{token: token, next: http.DefaultTransport}},
		MaxRetries: -1,
	}
	return mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(ctx, tr, nil)
}

func TestHTTPRightTokenListsToolsAndQueries(t *testing.T) {
	ts, _ := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := connectHTTP(ctx, ts.URL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	want := []string{"describe_table", "list_environments", "list_tables", "query"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "query", Arguments: map[string]any{"env": "dev", "sql": "select 1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool error: %+v", res.Content)
	}
}

func TestHTTPBadTokenIsRejected(t *testing.T) {
	for name, token := range map[string]string{
		"missing": "",
		"wrong":   strings.Repeat("x", 32),
	} {
		t.Run(name, func(t *testing.T) {
			ts, logs := newTestServer(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if cs, err := connectHTTP(ctx, ts.URL, token); err == nil {
				cs.Close()
				t.Fatal("client connected without a valid token")
			}

			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{}`))
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if len(body) > 64 {
				t.Fatalf("401 body too long: %q", body)
			}
			l := logs.String()
			if !strings.Contains(l, "unauthorized") {
				t.Fatalf("no log line for the rejected request: %q", l)
			}
			if token != "" && strings.Contains(l, token) {
				t.Fatalf("log contains the presented token: %q", l)
			}
			if strings.Contains(l, testToken) {
				t.Fatalf("log contains the server token: %q", l)
			}
		})
	}
}

func TestHTTPHealthzAndUnknownPaths(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		t.Fatalf("healthz = %d %q", resp.StatusCode, body)
	}
	for _, p := range []string{"/", "/mcp/", "/metrics", "/healthz/x"} {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+p, nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", p, resp.StatusCode)
		}
	}
}

func TestHTTPLimitsBodySize(t *testing.T) {
	ts, _ := newTestServer(t)
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", 2<<20) + `"}}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(big))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestServeHTTPRefusesBadToken(t *testing.T) {
	cfg := writeConfig(t)
	const name = "ADMINER_BRIDGE_TEST_TOKEN"
	for desc, val := range map[string]*string{
		"unset": nil,
		"empty": ptr(""),
		"short": ptr("short-secret-value-abc"),
	} {
		t.Run(desc, func(t *testing.T) {
			if val == nil {
				t.Setenv(name, "")
				os.Unsetenv(name)
			} else {
				t.Setenv(name, *val)
			}
			var out, errOut bytes.Buffer
			err := run([]string{"serve", "--http", "127.0.0.1:0", "--config", cfg, "--token-env", name}, &out, &errOut)
			if err == nil {
				t.Fatal("serve --http started with a bad token")
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("error does not name the variable: %v", err)
			}
			if val != nil && *val != "" && strings.Contains(err.Error(), *val) {
				t.Fatalf("error contains the token value: %v", err)
			}
		})
	}
}

func TestServeHTTPDefaultTokenEnv(t *testing.T) {
	cfg := writeConfig(t)
	t.Setenv("ADMINER_BRIDGE_TOKEN", "")
	var out, errOut bytes.Buffer
	err := run([]string{"serve", "--http", "127.0.0.1:0", "--config", cfg}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "ADMINER_BRIDGE_TOKEN") {
		t.Fatalf("err = %v, want an error naming ADMINER_BRIDGE_TOKEN", err)
	}
}

func TestUsageMentionsHTTP(t *testing.T) {
	var out, errOut bytes.Buffer
	_ = run([]string{"help"}, &out, &errOut)
	for _, s := range []string{"serve --http ADDR", "--token-env NAME"} {
		if !strings.Contains(errOut.String(), s) {
			t.Fatalf("usage lacks %q: %q", s, errOut.String())
		}
	}
}

func ptr(s string) *string { return &s }

func TestServeHTTPShutsDownOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var logs syncBuffer
	done := make(chan error, 1)
	go func() { done <- serveHTTP(ctx, "127.0.0.1:0", http.NotFoundHandler(), log.New(&logs, "", 0)) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "listening on 127.0.0.1:") {
		if time.Now().After(deadline) {
			t.Fatalf("no listen log line: %q", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serveHTTP = %v, want nil after shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveHTTP did not return after cancel")
	}
}

func TestHTTPBearerSchemeIsCaseInsensitive(t *testing.T) {
	ts, _ := newTestServer(t)
	const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"0"}}}`
	for header, want := range map[string]int{
		"Bearer " + testToken:               http.StatusOK,
		"bearer " + testToken:               http.StatusOK,
		"BEARER " + testToken:               http.StatusOK,
		"bearer " + strings.Repeat("x", 32): http.StatusUnauthorized,
		"Basic " + testToken:                http.StatusUnauthorized,
		testToken:                           http.StatusUnauthorized,
	} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(initBody))
		req.Header.Set("Authorization", header)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			scheme, _, _ := strings.Cut(header, " ")
			t.Errorf("scheme %q: status = %d, want %d", scheme, resp.StatusCode, want)
		}
	}
}
