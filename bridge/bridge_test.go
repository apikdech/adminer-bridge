package bridge

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"adminer-bridge/adminer"
	"adminer-bridge/config"
	"adminer-bridge/gate"
)

func testEnv() *config.Env {
	return &config.Env{
		Name: "dev", URL: "http://x/", Database: "db", Schema: "public",
		StatementTimeout: 15 * time.Second, LockTimeout: 2 * time.Second,
	}
}

func testConfig() *config.Config {
	return &config.Config{Environments: map[string]*config.Env{"dev": testEnv()}}
}

func str(s string) *string { return &s }

// stmtOf returns the caller statement (index gate.WrappedStmtIndex) of a wrapped batch.
func stmtOf(batch string) string {
	return strings.Split(batch, ";\n")[gate.WrappedStmtIndex]
}

func msg() adminer.Block { return adminer.Block{Kind: adminer.KindMessage, Message: "ok"} }

func tableBlocks(stmt string, cols []string, rows [][]*string) []adminer.Block {
	return []adminer.Block{msg(), msg(), msg(), msg(),
		{SQL: stmt, Kind: adminer.KindTable, Columns: cols, Rows: rows}}
}

type call struct {
	batch string
	limit int
}

// fake answers init queries from its fields and everything else via user.
type fake struct {
	mu        sync.Mutex
	calls     []call
	procs     []string
	super     string
	exts      []string
	langs     []string
	user      func(stmt string, limit int) []adminer.Block
	delay     time.Duration
	inflight  int32
	maxFlight int32
	initErr   error
}

func col(vals []string) [][]*string {
	var r [][]*string
	for _, v := range vals {
		r = append(r, []*string{str(v)})
	}
	return r
}

func (f *fake) Exec(ctx context.Context, batch string, limit int) ([]adminer.Block, error) {
	n := atomic.AddInt32(&f.inflight, 1)
	defer atomic.AddInt32(&f.inflight, -1)
	f.mu.Lock()
	if n > f.maxFlight {
		f.maxFlight = n
	}
	f.calls = append(f.calls, call{batch, limit})
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	stmt := stmtOf(batch)
	switch {
	case strings.Contains(stmt, "pg_proc"):
		if f.initErr != nil {
			return nil, f.initErr
		}
		return tableBlocks(stmt, []string{"proname"}, col(f.procs)), nil
	case strings.Contains(stmt, "rolsuper"):
		return tableBlocks(stmt, []string{"rolsuper"}, col([]string{f.super})), nil
	case strings.Contains(stmt, "pg_extension"):
		return tableBlocks(stmt, []string{"extname"}, col(f.exts)), nil
	case strings.Contains(stmt, "pg_language"):
		return tableBlocks(stmt, []string{"lanname"}, col(f.langs)), nil
	}
	if f.user != nil {
		return f.user(stmt, limit), nil
	}
	return tableBlocks(stmt, []string{"x"}, col([]string{"1"})), nil
}

// userCalls returns the calls that are not init queries.
func (f *fake) userCalls() []call {
	var out []call
	for _, c := range f.calls {
		s := stmtOf(c.batch)
		if strings.Contains(s, "pg_catalog.pg_") {
			continue
		}
		out = append(out, c)
	}
	return out
}

func newBridge(f *fake) *Bridge {
	return NewWithExecutors(testConfig(), map[string]Executor{"dev": f})
}

func TestQueryRejectedNeverReachesExecutor(t *testing.T) {
	f := &fake{super: "f"}
	b := newBridge(f)
	_, err := b.Query(context.Background(), "dev", "delete from users", 0)
	var rej *gate.RejectError
	if !errors.As(err, &rej) {
		t.Fatalf("want RejectError, got %v", err)
	}
	if n := len(f.userCalls()); n != 0 {
		t.Fatalf("executor got %d user batches", n)
	}
}

func TestFirstConnectLoadsFuncSetOnce(t *testing.T) {
	f := &fake{procs: []string{"count"}, super: "f"}
	b := newBridge(f)
	for i := 0; i < 2; i++ {
		if _, err := b.Query(context.Background(), "dev", "select count(*) from users", 0); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.calls) != 4+2 {
		t.Fatalf("want 6 batches (4 init + 2 user), got %d", len(f.calls))
	}
	for _, c := range f.calls[:4] {
		if c.limit != 0 {
			t.Errorf("init limit = %d, want 0", c.limit)
		}
	}
}

func TestInitRetriedAfterFailure(t *testing.T) {
	f := &fake{procs: []string{"count"}, super: "f", initErr: errors.New("boom")}
	b := newBridge(f)
	if _, err := b.Query(context.Background(), "dev", "select count(*) from users", 0); err == nil {
		t.Fatal("want init error")
	} else if !strings.Contains(err.Error(), "dev") {
		t.Errorf("error should name env: %v", err)
	}
	if b.ListEnvironments()[0].Connected {
		t.Fatal("must not be connected")
	}
	f.initErr = nil
	if _, err := b.Query(context.Background(), "dev", "select count(*) from users", 0); err != nil {
		t.Fatal(err)
	}
}

func TestFuncRulesUseLoadedSet(t *testing.T) {
	f := &fake{procs: []string{"count", "pg_sleep"}, super: "f"}
	b := newBridge(f)
	ctx := context.Background()
	if _, err := b.Query(ctx, "dev", "select count(*) from users", 0); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"select pg_sleep(1)", "select my_func()"} {
		_, err := b.Query(ctx, "dev", sql, 0)
		var rej *gate.RejectError
		if !errors.As(err, &rej) || rej.Rule != gate.RuleFunction {
			t.Errorf("%s: want function_not_allowed, got %v", sql, err)
		}
	}
}

func TestQuerySendsWrappedDeparsed(t *testing.T) {
	f := &fake{super: "f"}
	b := newBridge(f)
	res, err := b.Query(context.Background(), "dev", "select  *  from users where id=1", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := gate.Wrap("SELECT * FROM users WHERE id = 1", gate.Timeouts{Statement: 15 * time.Second, Lock: 2 * time.Second})
	uc := f.userCalls()
	if len(uc) != 1 || uc[0].batch != want {
		t.Fatalf("batch = %q, want %q", uc[0].batch, want)
	}
	if uc[0].limit != 101 {
		t.Errorf("limit = %d", uc[0].limit)
	}
	if res.ExecutedSQL != "SELECT * FROM users WHERE id = 1" {
		t.Errorf("ExecutedSQL = %q", res.ExecutedSQL)
	}
}

func TestTruncation(t *testing.T) {
	f := &fake{super: "f"}
	f.user = func(stmt string, limit int) []adminer.Block {
		n := limit
		vals := make([]string, n)
		for i := range vals {
			vals[i] = "v"
		}
		return tableBlocks(stmt, []string{"x"}, col(vals))
	}
	b := newBridge(f)
	res, err := b.Query(context.Background(), "dev", "select x from t", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 100 || res.RowCount != 100 || !res.Truncated {
		t.Fatalf("rows=%d count=%d trunc=%v", len(res.Rows), res.RowCount, res.Truncated)
	}
	if _, err := b.Query(context.Background(), "dev", "select x from t", 5000); err != nil {
		t.Fatal(err)
	}
	uc := f.userCalls()
	if uc[1].limit != 1001 {
		t.Errorf("limit = %d, want 1001", uc[1].limit)
	}
}

func TestNotTruncatedAtExactLimit(t *testing.T) {
	f := &fake{super: "f"}
	f.user = func(stmt string, limit int) []adminer.Block {
		return tableBlocks(stmt, []string{"x"}, col([]string{"a", "b", "c"}))
	}
	res, err := newBridge(f).Query(context.Background(), "dev", "select x from t", 3)
	if err != nil || res.Truncated || res.RowCount != 3 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestWrapperFailure(t *testing.T) {
	f := &fake{super: "f"}
	f.user = func(stmt string, limit int) []adminer.Block {
		bl := tableBlocks(stmt, []string{"x"}, nil)
		bl[1] = adminer.Block{Kind: adminer.KindError, Message: "Error in query (7): nope"}
		return bl
	}
	_, err := newBridge(f).Query(context.Background(), "dev", "select 1", 0)
	if err == nil || !strings.Contains(err.Error(), "read-only wrapper failed") {
		t.Fatalf("got %v", err)
	}
}

func TestEchoMismatch(t *testing.T) {
	f := &fake{super: "f"}
	f.user = func(stmt string, limit int) []adminer.Block {
		return tableBlocks("DELETE FROM users", []string{"x"}, nil)
	}
	_, err := newBridge(f).Query(context.Background(), "dev", "select 1", 0)
	if err == nil || !strings.Contains(err.Error(), "different statement") {
		t.Fatalf("got %v", err)
	}
}

func TestTruncatedEchoPrefix(t *testing.T) {
	f := &fake{super: "f"}
	f.user = func(stmt string, limit int) []adminer.Block {
		bl := tableBlocks(stmt[:5], []string{"x"}, col([]string{"1"}))
		bl[4].Truncated = true
		return bl
	}
	if _, err := newBridge(f).Query(context.Background(), "dev", "select 1", 0); err != nil {
		t.Fatalf("prefix should pass: %v", err)
	}
	f.user = func(stmt string, limit int) []adminer.Block {
		bl := tableBlocks("DELETE", []string{"x"}, nil)
		bl[4].Truncated = true
		return bl
	}
	if _, err := newBridge(f).Query(context.Background(), "dev", "select 1", 0); err == nil {
		t.Fatal("non-prefix must fail")
	}
}

func TestTooFewBlocks(t *testing.T) {
	f := &fake{super: "f"}
	f.user = func(stmt string, limit int) []adminer.Block { return []adminer.Block{msg()} }
	if _, err := newBridge(f).Query(context.Background(), "dev", "select 1", 0); err == nil {
		t.Fatal("want error")
	}
}

func TestStatementError(t *testing.T) {
	f := &fake{super: "f"}
	f.user = func(stmt string, limit int) []adminer.Block {
		bl := tableBlocks(stmt, nil, nil)
		bl[4] = adminer.Block{SQL: stmt, Kind: adminer.KindError, Message: `relation "x" does not exist`}
		return bl
	}
	_, err := newBridge(f).Query(context.Background(), "dev", "select * from x", 0)
	if err == nil || !strings.Contains(err.Error(), `relation "x" does not exist`) {
		t.Fatalf("got %v", err)
	}
}

func TestEmptyResult(t *testing.T) {
	f := &fake{super: "f"}
	f.user = func(stmt string, limit int) []adminer.Block {
		bl := tableBlocks(stmt, nil, nil)
		bl[4] = adminer.Block{SQL: stmt, Kind: adminer.KindMessage, Message: "No rows."}
		return bl
	}
	res, err := newBridge(f).Query(context.Background(), "dev", "select 1 where false", 0)
	if err != nil || res.RowCount != 0 || len(res.Columns) != 0 || res.Truncated {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestRoleWarnings(t *testing.T) {
	f := &fake{super: "t", exts: []string{"dblink", "plpgsql"}, langs: []string{"plpython3u"}}
	b := newBridge(f)
	if _, err := b.Query(context.Background(), "dev", "select 1", 0); err != nil {
		t.Fatal(err)
	}
	info := b.ListEnvironments()[0]
	if !info.Connected || len(info.Warnings) != 3 {
		t.Fatalf("%+v", info)
	}
	if info.Superuser == nil || !*info.Superuser {
		t.Errorf("superuser not set")
	}
}

func TestListEnvironmentsBeforeConnect(t *testing.T) {
	b := newBridge(&fake{})
	info := b.ListEnvironments()
	if len(info) != 1 || info[0].Name != "dev" || info[0].Connected || info[0].Database != "db" {
		t.Fatalf("%+v", info)
	}
}

func TestUnknownEnv(t *testing.T) {
	b := newBridge(&fake{})
	_, err := b.Query(context.Background(), "nope", "select 1", 0)
	if err == nil || !strings.Contains(err.Error(), "dev") {
		t.Fatalf("got %v", err)
	}
}

func TestSerializedPerEnv(t *testing.T) {
	f := &fake{super: "f", delay: 20 * time.Millisecond}
	b := newBridge(f)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Query(context.Background(), "dev", "select 1", 0); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.maxFlight != 1 {
		t.Fatalf("max in flight = %d", f.maxFlight)
	}
}
