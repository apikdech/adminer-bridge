package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
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
	db    string
	batch string
	limit int
}

// fake answers init queries from its fields and everything else via user.
type fake struct {
	mu        sync.Mutex
	calls     []call
	procs     []string
	dbProcs   map[string][]string // per-database procs; overrides procs when set
	missing   map[string]bool     // databases the preflight does not find
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

func (f *fake) Exec(ctx context.Context, db, batch string, limit int) ([]adminer.Block, error) {
	n := atomic.AddInt32(&f.inflight, 1)
	defer atomic.AddInt32(&f.inflight, -1)
	f.mu.Lock()
	if n > f.maxFlight {
		f.maxFlight = n
	}
	f.calls = append(f.calls, call{db, batch, limit})
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	stmt := stmtOf(batch)
	blocks, err := f.answer(db, stmt, limit)
	// Fill wrapper echoes from the batch actually sent.
	parts := strings.Split(batch, ";\n")
	for i := 0; i < 4 && i < len(blocks); i++ {
		if blocks[i].Kind == adminer.KindMessage && blocks[i].SQL == "" {
			blocks[i].SQL = parts[i]
		}
	}
	return blocks, err
}

var datnameRe = regexp.MustCompile(`datname = '([^']*)'`)

// isSetup reports whether stmt is an init or preflight query.
func isSetup(stmt string) bool {
	for _, m := range []string{"pg_proc", "rolsuper", "pg_extension", "pg_language", "pg_database"} {
		if strings.Contains(stmt, m) {
			return true
		}
	}
	return false
}

func (f *fake) answer(db, stmt string, limit int) ([]adminer.Block, error) {
	switch {
	case strings.Contains(stmt, "pg_proc"):
		if f.initErr != nil {
			return nil, f.initErr
		}
		procs := f.procs
		if f.dbProcs != nil {
			procs = f.dbProcs[db]
		}
		// The preflight needs this function in the default database.
		procs = append([]string{"has_database_privilege"}, procs...)
		return tableBlocks(stmt, []string{"proname"}, col(procs)), nil
	case strings.Contains(stmt, "pg_database"):
		m := datnameRe.FindStringSubmatch(stmt)
		if m == nil || f.missing[m[1]] {
			return tableBlocks(stmt, []string{"datname"}, nil), nil
		}
		return tableBlocks(stmt, []string{"datname"}, col([]string{m[1]})), nil
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
	_, err := b.Query(context.Background(), "dev", "", "delete from users", 0)
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
		if _, err := b.Query(context.Background(), "dev", "", "select count(*) from users", 0); err != nil {
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
	if _, err := b.Query(context.Background(), "dev", "", "select count(*) from users", 0); err == nil {
		t.Fatal("want init error")
	} else if !strings.Contains(err.Error(), "dev") {
		t.Errorf("error should name env: %v", err)
	}
	if b.ListEnvironments()[0].Databases[0].Connected {
		t.Fatal("must not be connected")
	}
	f.initErr = nil
	if _, err := b.Query(context.Background(), "dev", "", "select count(*) from users", 0); err != nil {
		t.Fatal(err)
	}
}

func TestFuncRulesUseLoadedSet(t *testing.T) {
	f := &fake{procs: []string{"count", "pg_sleep"}, super: "f"}
	b := newBridge(f)
	ctx := context.Background()
	if _, err := b.Query(ctx, "dev", "", "select count(*) from users", 0); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"select pg_sleep(1)", "select my_func()"} {
		_, err := b.Query(ctx, "dev", "", sql, 0)
		var rej *gate.RejectError
		if !errors.As(err, &rej) || rej.Rule != gate.RuleFunction {
			t.Errorf("%s: want function_not_allowed, got %v", sql, err)
		}
	}
}

func TestQuerySendsWrappedDeparsed(t *testing.T) {
	f := &fake{super: "f"}
	b := newBridge(f)
	res, err := b.Query(context.Background(), "dev", "", "select  *  from users where id=1", 0)
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
	res, err := b.Query(context.Background(), "dev", "", "select x from t", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 100 || res.RowCount != 100 || !res.Truncated {
		t.Fatalf("rows=%d count=%d trunc=%v", len(res.Rows), res.RowCount, res.Truncated)
	}
	if _, err := b.Query(context.Background(), "dev", "", "select x from t", 5000); err != nil {
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
	res, err := newBridge(f).Query(context.Background(), "dev", "", "select x from t", 3)
	if err != nil || res.Truncated || res.RowCount != 3 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestWrapperFailure(t *testing.T) {
	f := &fake{super: "f"}
	f.user = func(stmt string, limit int) []adminer.Block {
		return []adminer.Block{msg(), {Kind: adminer.KindError, Message: "Error in query (7): nope"}}
	}
	_, err := newBridge(f).Query(context.Background(), "dev", "", "select 1", 0)
	if err == nil || !strings.Contains(err.Error(), "read-only wrapper failed: Error in query (7): nope") {
		t.Fatalf("got %v", err)
	}
}

func TestEchoMismatch(t *testing.T) {
	f := &fake{super: "f"}
	f.user = func(stmt string, limit int) []adminer.Block {
		return tableBlocks("DELETE FROM users", []string{"x"}, nil)
	}
	_, err := newBridge(f).Query(context.Background(), "dev", "", "select 1", 0)
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
	if _, err := newBridge(f).Query(context.Background(), "dev", "", "select 1", 0); err != nil {
		t.Fatalf("prefix should pass: %v", err)
	}
	f.user = func(stmt string, limit int) []adminer.Block {
		bl := tableBlocks("DELETE", []string{"x"}, nil)
		bl[4].Truncated = true
		return bl
	}
	if _, err := newBridge(f).Query(context.Background(), "dev", "", "select 1", 0); err == nil {
		t.Fatal("non-prefix must fail")
	}
}

func TestTooFewBlocks(t *testing.T) {
	f := &fake{super: "f"}
	f.user = func(stmt string, limit int) []adminer.Block { return []adminer.Block{msg()} }
	if _, err := newBridge(f).Query(context.Background(), "dev", "", "select 1", 0); err == nil {
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
	_, err := newBridge(f).Query(context.Background(), "dev", "", "select * from x", 0)
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
	res, err := newBridge(f).Query(context.Background(), "dev", "", "select 1 where false", 0)
	if err != nil || res.RowCount != 0 || len(res.Columns) != 0 || res.Truncated {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestRoleWarnings(t *testing.T) {
	f := &fake{super: "t", exts: []string{"dblink", "plpgsql"}, langs: []string{"plpython3u"}}
	b := newBridge(f)
	if _, err := b.Query(context.Background(), "dev", "", "select 1", 0); err != nil {
		t.Fatal(err)
	}
	info := b.ListEnvironments()[0].Databases[0]
	if !info.Connected || len(info.Warnings) != 3 {
		t.Fatalf("%+v", info)
	}
	if info.Superuser == nil || !*info.Superuser {
		t.Errorf("superuser not set")
	}
	want := []string{
		"login role is a superuser; a dedicated read-only role is recommended",
		"risky extension installed: dblink",
		"untrusted language installed: plpython3u",
	}
	if !reflect.DeepEqual(info.Warnings, want) {
		t.Errorf("warnings = %q", info.Warnings)
	}
}

func TestSuperuserParsing(t *testing.T) {
	for v, want := range map[string]*bool{"t": ptr(true), "true": ptr(true), "1": ptr(true), "f": ptr(false), "false": ptr(false), "0": ptr(false), "x": nil} {
		f := &fake{super: v}
		b := newBridge(f)
		if _, err := b.Query(context.Background(), "dev", "", "select 1", 0); err != nil {
			t.Fatal(err)
		}
		info := b.ListEnvironments()[0].Databases[0]
		if (info.Superuser == nil) != (want == nil) || (want != nil && *info.Superuser != *want) {
			t.Errorf("%q: superuser = %v", v, info.Superuser)
		}
		if v == "x" && (len(info.Warnings) != 1 || info.Warnings[0] != "could not determine whether login role is a superuser (got x)") {
			t.Errorf("warnings = %q", info.Warnings)
		}
	}
}

func ptr(b bool) *bool { return &b }

func TestWrapperEchoMismatch(t *testing.T) {
	f := &fake{super: "f"}
	f.user = func(stmt string, limit int) []adminer.Block {
		bl := tableBlocks(stmt, []string{"x"}, nil)
		bl[0].SQL = "BEGIN"
		return bl
	}
	_, err := newBridge(f).Query(context.Background(), "dev", "", "select 1", 0)
	if err == nil || !strings.Contains(err.Error(), "read-only wrapper failed") {
		t.Fatalf("got %v", err)
	}
}

func TestTruncatedEmptyEcho(t *testing.T) {
	f := &fake{super: "f"}
	f.user = func(stmt string, limit int) []adminer.Block {
		bl := tableBlocks("", []string{"x"}, nil)
		bl[4].Truncated = true
		return bl
	}
	if _, err := newBridge(f).Query(context.Background(), "dev", "", "select 1", 0); err == nil {
		t.Fatal("empty truncated echo must fail")
	}
}

func TestListEnvironmentsDoesNotWaitForQuery(t *testing.T) {
	f := &fake{super: "f", delay: 300 * time.Millisecond}
	b := newBridge(f)
	go b.Query(context.Background(), "dev", "", "select 1", 0)
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	b.ListEnvironments()
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("ListEnvironments blocked for %v", d)
	}
}

func TestListEnvironmentsBeforeConnect(t *testing.T) {
	b := newBridge(&fake{})
	info := b.ListEnvironments()
	if len(info) != 1 || info[0].Name != "dev" || info[0].Database != "db" || info[0].Schema != "public" {
		t.Fatalf("%+v", info)
	}
	want := []DatabaseInfo{{Name: "db"}}
	if !reflect.DeepEqual(info[0].Databases, want) {
		t.Fatalf("databases = %+v, want %+v", info[0].Databases, want)
	}
}

func TestUnknownEnv(t *testing.T) {
	b := newBridge(&fake{})
	_, err := b.Query(context.Background(), "nope", "", "select 1", 0)
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
			if _, err := b.Query(context.Background(), "dev", "", "select 1", 0); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.maxFlight != 1 {
		t.Fatalf("max in flight = %d", f.maxFlight)
	}
}

// multiBridge has one environment "dev" with default database "db" and
// allowlist [db, db2].
func multiBridge(f *fake) *Bridge {
	env := testEnv()
	env.Databases = []string{"db", "db2"}
	cfg := &config.Config{Environments: map[string]*config.Env{"dev": env}}
	return NewWithExecutors(cfg, map[string]Executor{"dev": f})
}

func TestDefaultDatabaseWhenEmpty(t *testing.T) {
	f := &fake{super: "f"}
	b := multiBridge(f)
	res, err := b.Query(context.Background(), "dev", "", "select 1", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if c.db != "db" {
			t.Errorf("batch sent to %q, want the default database db", c.db)
		}
	}
	if res.Database != "db" {
		t.Errorf("Result.Database = %q, want db", res.Database)
	}
}

func TestExplicitDatabaseReachesExecutor(t *testing.T) {
	f := &fake{super: "f"}
	b := multiBridge(f)
	res, err := b.Query(context.Background(), "dev", "db2", "select 1", 0)
	if err != nil {
		t.Fatal(err)
	}
	// 4 init + 1 preflight in the default database, then 4 init + 1 user in db2.
	var dbs []string
	for _, c := range f.calls {
		dbs = append(dbs, c.db)
	}
	want := []string{"db", "db", "db", "db", "db", "db2", "db2", "db2", "db2", "db2"}
	if !reflect.DeepEqual(dbs, want) {
		t.Fatalf("batch databases = %v, want %v", dbs, want)
	}
	if !strings.Contains(stmtOf(f.calls[4].batch), "pg_database") {
		t.Errorf("call 4 = %s, want the preflight", stmtOf(f.calls[4].batch))
	}
	if res.Database != "db2" {
		t.Errorf("Result.Database = %q, want db2", res.Database)
	}
}

func TestDisallowedDatabaseNeverReachesExecutor(t *testing.T) {
	f := &fake{super: "f"}
	b := multiBridge(f)
	ctx := context.Background()
	want := `database "db3" is not allowed for environment "dev"; allowed: db, db2`
	check := func(name string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", name, err, want)
		}
	}
	_, err := b.Query(ctx, "dev", "db3", "select 1", 0)
	check("Query", err)
	_, err = b.ListTables(ctx, "dev", "db3", "")
	check("ListTables", err)
	_, err = b.DescribeTable(ctx, "dev", "db3", "users")
	check("DescribeTable", err)
	// Case-sensitive: "DB2" is not "db2".
	_, err = b.Query(ctx, "dev", "DB2", "select 1", 0)
	if err == nil || !strings.Contains(err.Error(), `database "DB2" is not allowed`) {
		t.Errorf("DB2: err = %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("executor got %d batches, want 0", len(f.calls))
	}
}

func TestDisallowedDatabaseCheckedBeforeGate(t *testing.T) {
	f := &fake{super: "f"}
	_, err := multiBridge(f).Query(context.Background(), "dev", "db3", "delete from users", 0)
	var rej *gate.RejectError
	if errors.As(err, &rej) || err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("err = %v, want the allowlist error", err)
	}
}

func TestInitRunsPerDatabase(t *testing.T) {
	f := &fake{super: "f", dbProcs: map[string][]string{"db": {"count"}, "db2": {"now"}}}
	b := multiBridge(f)
	ctx := context.Background()
	if _, err := b.Query(ctx, "dev", "", "select count(*) from t", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Query(ctx, "dev", "db2", "select now()", 0); err != nil {
		t.Fatal(err)
	}
	// Each database has its own FuncSet.
	var rej *gate.RejectError
	if _, err := b.Query(ctx, "dev", "db2", "select count(*) from t", 0); !errors.As(err, &rej) || rej.Rule != gate.RuleFunction {
		t.Errorf("count in db2: want function_not_allowed, got %v", err)
	}
	if _, err := b.Query(ctx, "dev", "db", "select now()", 0); !errors.As(err, &rej) || rej.Rule != gate.RuleFunction {
		t.Errorf("now in db: want function_not_allowed, got %v", err)
	}
	// Init ran once per database: 4 + 4 init batches, 1 preflight in db, 2 user batches.
	inits := map[string]int{}
	for _, c := range f.calls {
		if isSetup(stmtOf(c.batch)) {
			inits[c.db]++
		}
	}
	if inits["db"] != 5 || inits["db2"] != 4 || len(f.calls) != 11 {
		t.Fatalf("setup batches = %v, total = %d; want db 5, db2 4, 11 total", inits, len(f.calls))
	}
	envs := b.ListEnvironments()
	if len(envs) != 1 || len(envs[0].Databases) != 2 {
		t.Fatalf("%+v", envs)
	}
	for i, name := range []string{"db", "db2"} {
		if d := envs[0].Databases[i]; d.Name != name || !d.Connected {
			t.Errorf("databases[%d] = %+v, want %s connected", i, d, name)
		}
	}
}

func TestInitFailureIsPerDatabase(t *testing.T) {
	f := &fake{super: "f"}
	b := multiBridge(f)
	ctx := context.Background()
	if _, err := b.Query(ctx, "dev", "", "select 1", 0); err != nil {
		t.Fatal(err)
	}
	f.initErr = errors.New("boom")
	_, err := b.Query(ctx, "dev", "db2", "select 1", 0)
	if err == nil || !strings.Contains(err.Error(), `"dev"`) || !strings.Contains(err.Error(), `"db2"`) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want env, database and cause", err)
	}
	dbs := b.ListEnvironments()[0].Databases
	if !dbs[0].Connected || dbs[1].Connected {
		t.Fatalf("databases = %+v, want db connected, db2 not", dbs)
	}
	// db still works; db2 retries init on the next call.
	if _, err := b.Query(ctx, "dev", "db", "select 1", 0); err != nil {
		t.Fatal(err)
	}
	f.initErr = nil
	if _, err := b.Query(ctx, "dev", "db2", "select 1", 0); err != nil {
		t.Fatal(err)
	}
	if !b.ListEnvironments()[0].Databases[1].Connected {
		t.Fatal("db2 not connected after retry")
	}
}

func TestExecutorErrorNamesEnvAndDatabase(t *testing.T) {
	f := &fake{super: "f"}
	f.user = func(stmt string, limit int) []adminer.Block {
		bl := tableBlocks(stmt, nil, nil)
		bl[4] = adminer.Block{SQL: stmt, Kind: adminer.KindError, Message: "nope"}
		return bl
	}
	_, err := multiBridge(f).Query(context.Background(), "dev", "db2", "select 1", 0)
	if err == nil || !strings.Contains(err.Error(), `environment "dev"`) || !strings.Contains(err.Error(), `database "db2"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestSerializedPerEnvAcrossDatabases(t *testing.T) {
	f := &fake{super: "f", delay: 20 * time.Millisecond}
	b := multiBridge(f)
	var wg sync.WaitGroup
	for _, db := range []string{"db", "db2", "db", "db2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Query(context.Background(), "dev", db, "select 1", 0); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.maxFlight != 1 {
		t.Fatalf("max in flight = %d, want 1", f.maxFlight)
	}
}

func TestListEnvironmentsJSONShape(t *testing.T) {
	f := &fake{super: "t"}
	b := multiBridge(f)
	if _, err := b.Query(context.Background(), "dev", "", "select 1", 0); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(b.ListEnvironments())
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"name":"dev","url":"http://x/","schema":"public","database":"db","databases":[` +
		`{"name":"db","connected":true,"superuser":true,"warnings":["login role is a superuser; a dedicated read-only role is recommended"]},` +
		`{"name":"db2","connected":false}]}]`
	if string(raw) != want {
		t.Fatalf("got  %s\nwant %s", raw, want)
	}
}

const missingErr = `database "db2" (environment "dev") does not exist or the login role cannot connect to it`

func TestPreflightRejectsMissingDatabase(t *testing.T) {
	f := &fake{super: "f", missing: map[string]bool{"db2": true}}
	b := multiBridge(f)
	ctx := context.Background()
	for _, call := range []func() error{
		func() error { _, err := b.Query(ctx, "dev", "db2", "select 1", 0); return err },
		func() error { _, err := b.ListTables(ctx, "dev", "db2", ""); return err },
		func() error { _, err := b.DescribeTable(ctx, "dev", "db2", "users"); return err },
	} {
		if err := call(); err == nil || err.Error() != missingErr {
			t.Errorf("err = %v, want %q", err, missingErr)
		}
	}
	for _, c := range f.calls {
		if c.db != "db" {
			t.Errorf("batch sent to %q; nothing may reach the missing database", c.db)
		}
	}
	// One default init (4) and one preflight: the negative result is cached.
	if len(f.calls) != 5 {
		t.Errorf("calls = %d, want 5", len(f.calls))
	}
	if !strings.Contains(stmtOf(f.calls[4].batch), "'db2'") {
		t.Errorf("preflight = %s", stmtOf(f.calls[4].batch))
	}
	if d := b.ListEnvironments()[0].Databases[1]; d.Connected {
		t.Errorf("db2 = %+v, want not connected", d)
	}
}

func TestPreflightPositiveRunsOnce(t *testing.T) {
	f := &fake{super: "f"}
	b := multiBridge(f)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := b.Query(ctx, "dev", "db2", "select 1", 0); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, c := range f.calls {
		if strings.Contains(stmtOf(c.batch), "pg_database") {
			n++
			if c.db != "db" {
				t.Errorf("preflight sent to %q, want the default database", c.db)
			}
		}
	}
	if n != 1 {
		t.Errorf("preflights = %d, want 1", n)
	}
}

func TestPreflightNotForDefaultDatabase(t *testing.T) {
	f := &fake{super: "f"}
	b := multiBridge(f)
	if _, err := b.Query(context.Background(), "dev", "", "select 1", 0); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.Contains(stmtOf(c.batch), "pg_database") {
			t.Fatal("preflight ran for the default database")
		}
	}
}

func TestPreflightRetriedAfterInitFailure(t *testing.T) {
	// A passed preflight is kept even if the init of db2 then fails.
	f := &fake{super: "f"}
	b := multiBridge(f)
	ctx := context.Background()
	if _, err := b.Query(ctx, "dev", "", "select 1", 0); err != nil {
		t.Fatal(err)
	}
	f.initErr = errors.New("boom")
	if _, err := b.Query(ctx, "dev", "db2", "select 1", 0); err == nil {
		t.Fatal("want init error")
	}
	f.initErr = nil
	if _, err := b.Query(ctx, "dev", "db2", "select 1", 0); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range f.calls {
		if strings.Contains(stmtOf(c.batch), "pg_database") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("preflights = %d, want 1", n)
	}
}

func TestPreflightNegativeCacheExpires(t *testing.T) {
	f := &fake{super: "f", missing: map[string]bool{"db2": true}}
	b := multiBridge(f)
	now := time.Now()
	b.now = func() time.Time { return now }
	ctx := context.Background()
	preflights := func() int {
		n := 0
		for _, c := range f.calls {
			if strings.Contains(stmtOf(c.batch), "pg_database") {
				n++
			}
		}
		return n
	}
	if _, err := b.Query(ctx, "dev", "db2", "select 1", 0); err == nil || err.Error() != missingErr {
		t.Fatalf("err = %v", err)
	}
	now = now.Add(59 * time.Second)
	if _, err := b.Query(ctx, "dev", "db2", "select 1", 0); err == nil || err.Error() != missingErr {
		t.Fatalf("cached err = %v", err)
	}
	if n := preflights(); n != 1 {
		t.Fatalf("preflights within 60s = %d, want 1", n)
	}
	// The database is created; after 60s the bridge checks again.
	now = now.Add(2 * time.Second)
	f.missing = nil
	if _, err := b.Query(ctx, "dev", "db2", "select 1", 0); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if n := preflights(); n != 2 {
		t.Fatalf("preflights = %d, want 2", n)
	}
}

func TestPreflightNeedsDefaultInit(t *testing.T) {
	// If the default database cannot be initialised, nothing is sent to db2.
	f := &fake{super: "f", initErr: errors.New("boom")}
	b := multiBridge(f)
	_, err := b.Query(context.Background(), "dev", "db2", "select 1", 0)
	if err == nil || !strings.Contains(err.Error(), `database "db"`) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the default database init error", err)
	}
	for _, c := range f.calls {
		if c.db != "db" {
			t.Errorf("batch sent to %q", c.db)
		}
	}
}

func TestPreflightStatementPassesGate(t *testing.T) {
	q := preflightSQL("o'brien")
	if _, err := gate.Validate(q, gate.FuncSet{"has_database_privilege": {}}); err != nil {
		t.Fatalf("preflight %q rejected: %v", q, err)
	}
	if !strings.Contains(q, "'o''brien'") {
		t.Errorf("name not quoted: %s", q)
	}
}
