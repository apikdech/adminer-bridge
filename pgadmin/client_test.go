package pgadmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apikdech/adminer-bridge/adminer"
	"github.com/apikdech/adminer-bridge/config"
)

// fakePgAdmin mimics the pgAdmin 9.8 endpoints the client uses. The JSON
// shapes follow responses captured from pgAdmin 9.8 (see testdata/NOTES.md).
type fakePgAdmin struct {
	t  *testing.T
	mu sync.Mutex

	password    string
	sessions    map[string]bool // session cookie -> logged in
	loginPosts  int
	connects    int
	inits       int
	sqls        []string // every statement started, in order
	readOnly    string   // what transaction_read_only returns inside a transaction
	tx          int      // 0 idle, 2 in transaction, 3 aborted
	pending     map[string]any
	expireNext  bool // answer the next API call with 401 and drop all sessions
	busyForever bool // every poll answers Busy
	serverLabel string
	results     map[string]fakeResult // by SQL text
}

type fakeResult struct {
	cols []string
	rows [][]any
	err  string
}

const fakeCSRF = "csrf-token-1"

func newFake(t *testing.T) (*fakePgAdmin, *httptest.Server) {
	f := &fakePgAdmin{
		t: t, password: "pw", sessions: map[string]bool{}, readOnly: "on",
		serverLabel: "koi-dev", results: map[string]fakeResult{},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakePgAdmin) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func ok(data any) map[string]any {
	return map[string]any{"success": 1, "errormsg": "", "info": "", "result": nil, "data": data}
}

func (f *fakePgAdmin) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case p == "/login" && r.Method == http.MethodGet:
		// pgAdmin 9.8 gives the token only to a JSON request; the HTML
		// login page has none.
		http.SetCookie(w, &http.Cookie{Name: "pga4_session", Value: "anon", Path: "/"})
		if !strings.Contains(r.Header.Get("Accept"), "application/json") {
			fmt.Fprint(w, "<!DOCTYPE html><html><body>login</body></html>")
			return
		}
		f.writeJSON(w, 200, map[string]any{"meta": map[string]any{"code": 200}, "response": map[string]any{"csrf_token": fakeCSRF, "identity_attributes": []string{"email"}}})
		return
	case p == "/authenticate/login" && r.Method == http.MethodPost:
		f.loginPosts++
		_ = r.ParseForm()
		if r.PostForm.Get("csrf_token") != fakeCSRF || r.PostForm.Get("password") != f.password || r.PostForm.Get("email") != "bot@example.com" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		sid := "s" + strconv.Itoa(f.loginPosts)
		f.sessions[sid] = true
		http.SetCookie(w, &http.Cookie{Name: "pga4_session", Value: sid, Path: "/"})
		http.Redirect(w, r, "/browser/", http.StatusFound)
		return
	case p == "/browser/":
		fmt.Fprint(w, "<html>browser</html>")
		return
	}
	// Every other endpoint is an API call: session and CSRF header required.
	c, err := r.Cookie("pga4_session")
	if f.expireNext {
		f.expireNext = false
		f.sessions = map[string]bool{}
	}
	if err != nil || !f.sessions[c.Value] {
		f.writeJSON(w, http.StatusUnauthorized, map[string]any{"success": 0, "errormsg": "Unauthorized", "info": "", "result": nil, "data": nil})
		return
	}
	// pgAdmin 9.8 over HTTPS checks the referrer strictly and answers 401.
	if !strings.HasPrefix(r.Header.Get("Referer"), "http://"+r.Host+"/") {
		f.writeJSON(w, http.StatusUnauthorized, map[string]any{"success": 0, "errormsg": "The referrer header is missing. You need to refresh the page.", "data": nil})
		return
	}
	if r.Header.Get("X-pgA-CSRFToken") != fakeCSRF {
		f.writeJSON(w, http.StatusBadRequest, map[string]any{"success": 0, "errormsg": "The CSRF token is missing.", "data": nil})
		return
	}
	parts := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	case p == "/browser/server_group/nodes/":
		f.writeJSON(w, 200, ok([]map[string]any{{"label": "Servers", "_id": "6", "_type": "server_group"}}))
	case p == "/browser/server/nodes/6":
		f.writeJSON(w, 200, map[string]any{"success": 1, "errormsg": "", "data": nil, "result": []map[string]any{
			{"label": f.serverLabel, "_id": "14", "connected": f.connects > 0, "is_password_saved": true, "username": "glchat_team"},
			{"label": "koi-demo", "_id": "16", "connected": false, "is_password_saved": true, "username": "glchat_team"},
		}})
	case p == "/browser/server/connect/6/14" && r.Method == http.MethodPost:
		f.connects++
		f.writeJSON(w, 200, map[string]any{"success": 1, "info": "Server connected.", "data": map[string]any{"connected": true}})
	case p == "/browser/database/nodes/6/14/":
		f.writeJSON(w, 200, ok([]map[string]any{
			{"label": "postgres", "_id": 5}, {"label": "glchat_dev", "_id": 2165387}, {"label": "other_db", "_id": 99},
		}))
	case len(parts) == 7 && parts[0] == "sqleditor" && parts[1] == "initialize":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if parts[6] != "2165387" && parts[6] != "99" || body["dbname"] == nil {
			f.writeJSON(w, 500, map[string]any{"success": 0, "errormsg": "'dbname'", "data": nil})
			return
		}
		f.inits++
		f.writeJSON(w, 200, ok(map[string]any{"connId": parts[3], "serverVersion": 170006}))
	case len(parts) == 4 && parts[0] == "sqleditor" && parts[1] == "query_tool" && parts[2] == "start":
		var body struct {
			SQL string `json:"sql"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.sqls = append(f.sqls, body.SQL)
		f.pending = f.execute(body.SQL)
		f.writeJSON(w, 200, ok(map[string]any{"status": -1, "result": nil}))
	case len(parts) == 3 && parts[0] == "sqleditor" && parts[1] == "poll":
		if f.busyForever {
			f.writeJSON(w, 200, ok(map[string]any{"status": "Busy", "result": nil}))
			return
		}
		res := f.pending
		f.pending = nil
		if res == nil {
			f.writeJSON(w, 200, ok(map[string]any{"status": "NotConnected", "result": nil}))
			return
		}
		if msg, ok := res["error"].(string); ok {
			f.writeJSON(w, 500, map[string]any{"success": 0, "errormsg": msg, "data": map[string]any{"transaction_status": f.tx}})
			return
		}
		f.writeJSON(w, 200, ok(res))
	case len(parts) == 5 && parts[0] == "sqleditor" && parts[1] == "fetch_window":
		from, _ := strconv.Atoi(parts[3])
		to, _ := strconv.Atoi(parts[4])
		rows := []any{}
		for i := from; i <= to && i <= 1500; i++ {
			rows = append(rows, []any{i})
		}
		f.writeJSON(w, 200, ok(map[string]any{"status": "Success", "result": rows, "row_count": len(rows)}))
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, p)
		http.NotFound(w, r)
	}
}

// execute returns the poll data of sql, following pgAdmin 9.8's shapes.
func (f *fakePgAdmin) execute(sql string) map[string]any {
	tag := func(t string) map[string]any {
		return map[string]any{"status": "Success", "result": t, "colinfo": nil, "rows_affected": 0, "transaction_status": f.tx}
	}
	if f.tx == 3 && sql != "ROLLBACK" {
		return map[string]any{"error": "ERROR:  current transaction is aborted, commands ignored until end of transaction block"}
	}
	switch {
	case sql == "BEGIN TRANSACTION READ ONLY":
		f.tx = 2
		return tag("BEGIN")
	case strings.HasPrefix(sql, "SET LOCAL "):
		return tag("SET")
	case sql == "ROLLBACK":
		f.tx = 0
		return tag("ROLLBACK")
	case sql == "SELECT pg_catalog.current_setting('transaction_read_only')":
		v := "off"
		if f.tx == 2 {
			v = f.readOnly
		}
		return table([]string{"current_setting"}, [][]any{{v}}, 1)
	case sql == "SELECT g FROM generate_series(1, 1500) g":
		rows := [][]any{}
		for i := 1; i <= 1000; i++ {
			rows = append(rows, []any{i})
		}
		return table([]string{"g"}, rows, 1500)
	}
	if r, ok := f.results[sql]; ok {
		if r.err != "" {
			if f.tx == 2 {
				f.tx = 3
			}
			return map[string]any{"error": r.err}
		}
		return table(r.cols, r.rows, len(r.rows))
	}
	f.t.Errorf("fake: no result for %q", sql)
	return map[string]any{"error": "fake: unknown statement"}
}

func table(cols []string, rows [][]any, total int) map[string]any {
	info := make([]map[string]any, len(cols))
	for i, c := range cols {
		info[i] = map[string]any{"name": c, "display_name": c, "pos": i, "type_code": 25}
	}
	var result any = rows
	if len(rows) == 0 {
		result = "SELECT 0"
	}
	return map[string]any{"status": "Success", "result": result, "colinfo": info, "rows_affected": total,
		"rows_fetched_from": 1, "rows_fetched_to": len(rows), "transaction_status": 2}
}

func testEnv(url string) *config.Env {
	return &config.Env{
		Name: "claudia-dev", Backend: config.BackendPgAdmin, URL: url, Server: "koi-dev",
		Username: "bot@example.com", Database: "glchat_dev", Schema: "public", PasswordEnv: "PGA_TEST_PW",
		StatementTimeout: 15 * time.Second, LockTimeout: 2 * time.Second, HTTPTimeout: 20 * time.Second,
	}
}

func newClient(t *testing.T, url, pw string) *Client {
	t.Helper()
	t.Setenv("PGA_TEST_PW", pw)
	c, err := NewClient(testEnv(url))
	if err != nil {
		t.Fatal(err)
	}
	c.pollEvery = time.Millisecond
	return c
}

const stmt = "SELECT id, name, active, score, meta, note FROM t"

func wrapped(s string) string {
	return strings.Join([]string{
		"BEGIN TRANSACTION READ ONLY",
		"SET LOCAL statement_timeout = '15000ms'",
		"SET LOCAL lock_timeout = '2000ms'",
		"SET LOCAL idle_in_transaction_session_timeout = '15000ms'",
		s,
		"ROLLBACK",
	}, ";\n") + ";"
}

func TestExecRunsWrappedBatchStepByStep(t *testing.T) {
	f, srv := newFake(t)
	f.results[stmt] = fakeResult{
		cols: []string{"id", "name", "active", "score", "meta", "note"},
		rows: [][]any{{json.Number("7"), "a", true, "1.5", map[string]any{"k": 1}, nil}},
	}
	c := newClient(t, srv.URL, "pw")
	blocks, err := c.Exec(context.Background(), "glchat_dev", wrapped(stmt), 101)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 6 {
		t.Fatalf("blocks = %d, want 6", len(blocks))
	}
	want := strings.Split(strings.TrimSuffix(wrapped(stmt), ";"), ";\n")
	for i, b := range blocks {
		if b.SQL != want[i] {
			t.Errorf("block %d SQL = %q, want %q", i, b.SQL, want[i])
		}
	}
	for _, i := range []int{0, 1, 2, 3, 5} {
		if blocks[i].Kind != adminer.KindMessage {
			t.Errorf("block %d kind = %v", i, blocks[i].Kind)
		}
	}
	b := blocks[4]
	if b.Kind != adminer.KindTable || strings.Join(b.Columns, ",") != "id,name,active,score,meta,note" {
		t.Fatalf("table block = %+v", b)
	}
	var got []string
	for _, v := range b.Rows[0] {
		if v == nil {
			got = append(got, "<nil>")
		} else {
			got = append(got, *v)
		}
	}
	if strings.Join(got, "|") != `7|a|true|1.5|{"k":1}|<nil>` {
		t.Errorf("row = %q", got)
	}
	// The read-only check runs right before the caller's statement.
	sent := strings.Join(f.sqls, "\n")
	wantSent := strings.Join(append(append(append([]string{}, want[:4]...), readOnlyCheck), want[4:]...), "\n")
	if sent != wantSent {
		t.Errorf("sent:\n%s\nwant:\n%s", sent, wantSent)
	}
	if f.tx != 0 {
		t.Errorf("transaction left open: %d", f.tx)
	}
}

func TestExecRefusesWhenNotReadOnly(t *testing.T) {
	f, srv := newFake(t)
	f.readOnly = "off"
	c := newClient(t, srv.URL, "pw")
	_, err := c.Exec(context.Background(), "glchat_dev", wrapped(stmt), 101)
	if err == nil || !strings.Contains(err.Error(), "not read-only") {
		t.Fatalf("err = %v", err)
	}
	for _, s := range f.sqls {
		if s == stmt {
			t.Fatal("statement ran outside a read-only transaction")
		}
	}
	if last := f.sqls[len(f.sqls)-1]; last != "ROLLBACK" || f.tx != 0 {
		t.Errorf("last sent = %q, tx = %d; want ROLLBACK", last, f.tx)
	}
}

func TestExecStatementErrorRollsBack(t *testing.T) {
	f, srv := newFake(t)
	f.results[stmt] = fakeResult{err: "ERROR:  relation \"t\" does not exist\nLINE 1: SELECT ...\n\nSQL state: 42P01"}
	c := newClient(t, srv.URL, "pw")
	blocks, err := c.Exec(context.Background(), "glchat_dev", wrapped(stmt), 101)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 5 || blocks[4].Kind != adminer.KindError || !strings.Contains(blocks[4].Message, `relation "t" does not exist`) {
		t.Fatalf("blocks = %+v", blocks)
	}
	if last := f.sqls[len(f.sqls)-1]; last != "ROLLBACK" || f.tx != 0 {
		t.Errorf("last sent = %q, tx = %d; want ROLLBACK", last, f.tx)
	}
}

func TestExecRefusesUnwrappedBatch(t *testing.T) {
	f, srv := newFake(t)
	c := newClient(t, srv.URL, "pw")
	for _, batch := range []string{"SELECT 1;", "BEGIN;\nSELECT 1;\nROLLBACK;", strings.Replace(wrapped(stmt), "ROLLBACK", "COMMIT", 1)} {
		if _, err := c.Exec(context.Background(), "glchat_dev", batch, 10); err == nil {
			t.Errorf("batch %q accepted", batch)
		}
	}
	if len(f.sqls) != 0 || f.loginPosts != 0 {
		t.Errorf("sent requests for a refused batch: sqls %v, logins %d", f.sqls, f.loginPosts)
	}
}

func TestLoginFailureIsLatched(t *testing.T) {
	f, srv := newFake(t)
	c := newClient(t, srv.URL, "bad-pw-7q3")
	for i := 0; i < 3; i++ {
		_, err := c.Exec(context.Background(), "glchat_dev", wrapped(stmt), 10)
		if !errors.Is(err, ErrLogin) {
			t.Fatalf("call %d: err = %v", i, err)
		}
		if strings.Contains(err.Error(), "bad-pw-7q3") {
			t.Fatalf("error leaks the password: %v", err)
		}
	}
	if f.loginPosts != 1 {
		t.Errorf("login posts = %d, want 1 (pgAdmin locks accounts after failed logins)", f.loginPosts)
	}
}

func TestSessionExpiryLogsInAgainOnce(t *testing.T) {
	f, srv := newFake(t)
	f.results[stmt] = fakeResult{cols: []string{"x"}, rows: [][]any{{"1"}}}
	c := newClient(t, srv.URL, "pw")
	if _, err := c.Exec(context.Background(), "glchat_dev", wrapped(stmt), 10); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.expireNext = true
	f.mu.Unlock()
	blocks, err := c.Exec(context.Background(), "glchat_dev", wrapped(stmt), 10)
	if err != nil {
		t.Fatal(err)
	}
	if blocks[4].Kind != adminer.KindTable {
		t.Errorf("block = %+v", blocks[4])
	}
	if f.loginPosts != 2 || f.inits != 2 {
		t.Errorf("logins = %d, inits = %d; want 2 and 2", f.loginPosts, f.inits)
	}
}

func TestQueryToolReusedPerDatabase(t *testing.T) {
	f, srv := newFake(t)
	f.results[stmt] = fakeResult{cols: []string{"x"}, rows: [][]any{{"1"}}}
	c := newClient(t, srv.URL, "pw")
	for i := 0; i < 3; i++ {
		if _, err := c.Exec(context.Background(), "glchat_dev", wrapped(stmt), 10); err != nil {
			t.Fatal(err)
		}
	}
	if f.loginPosts != 1 || f.connects != 1 || f.inits != 1 {
		t.Errorf("logins %d, connects %d, inits %d; want 1 each", f.loginPosts, f.connects, f.inits)
	}
	if _, err := c.Exec(context.Background(), "other_db", wrapped(stmt), 10); err != nil {
		t.Fatal(err)
	}
	if f.inits != 2 {
		t.Errorf("inits = %d after a second database, want 2", f.inits)
	}
}

func TestPagingPastFirstFetch(t *testing.T) {
	big := "SELECT g FROM generate_series(1, 1500) g"
	for _, tc := range []struct{ limit, want int }{{1001, 1001}, {0, 1500}, {10, 10}} {
		_, srv := newFake(t)
		c := newClient(t, srv.URL, "pw")
		blocks, err := c.Exec(context.Background(), "glchat_dev", wrapped(big), tc.limit)
		if err != nil {
			t.Fatal(err)
		}
		rows := blocks[4].Rows
		if len(rows) != tc.want {
			t.Fatalf("limit %d: rows = %d, want %d", tc.limit, len(rows), tc.want)
		}
		if last := *rows[len(rows)-1][0]; last != strconv.Itoa(tc.want) {
			t.Errorf("limit %d: last row = %s", tc.limit, last)
		}
	}
}

func TestUnknownServerAndDatabase(t *testing.T) {
	f, srv := newFake(t)
	f.serverLabel = "renamed"
	c := newClient(t, srv.URL, "pw")
	_, err := c.Exec(context.Background(), "glchat_dev", wrapped(stmt), 10)
	if err == nil || !strings.Contains(err.Error(), `server "koi-dev"`) || !strings.Contains(err.Error(), "renamed") {
		t.Fatalf("err = %v", err)
	}

	_, srv2 := newFake(t)
	c2 := newClient(t, srv2.URL, "pw")
	_, err = c2.Exec(context.Background(), "nope_db", wrapped(stmt), 10)
	if err == nil || !strings.Contains(err.Error(), `database "nope_db"`) {
		t.Fatalf("err = %v", err)
	}
}

// pgAdmin 9.8 answers a SELECT without rows with colinfo and the command
// tag "SELECT 0" in result, not an empty list.
func TestZeroRowSelect(t *testing.T) {
	f, srv := newFake(t)
	f.results[stmt] = fakeResult{cols: []string{"lanname"}, rows: nil}
	c := newClient(t, srv.URL, "pw")
	blocks, err := c.Exec(context.Background(), "glchat_dev", wrapped(stmt), 10)
	if err != nil {
		t.Fatal(err)
	}
	if b := blocks[4]; b.Kind != adminer.KindTable || len(b.Rows) != 0 || len(b.Columns) != 1 {
		t.Fatalf("block = %+v", b)
	}
}

func TestBusyPollingIsBounded(t *testing.T) {
	f, fsrv := newFake(t)
	f.results[stmt] = fakeResult{cols: []string{"x"}, rows: [][]any{{"1"}}}
	c := newClient(t, fsrv.URL, "pw")
	if _, err := c.Exec(context.Background(), "glchat_dev", wrapped(stmt), 10); err != nil {
		t.Fatal(err)
	}
	// From now on every poll says Busy.
	f.mu.Lock()
	f.busyForever = true
	f.mu.Unlock()
	clock := time.Now()
	c.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	_, err := c.Exec(context.Background(), "glchat_dev", wrapped(stmt), 10)
	if err == nil || !strings.Contains(err.Error(), "still busy") {
		t.Fatalf("err = %v", err)
	}
}
