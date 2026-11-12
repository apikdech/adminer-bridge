// Package bridge ties config, gate and adminer together: it is the only way
// callers run SQL. Every statement, including the bridge's own catalog
// queries, passes gate.Validate before it is wrapped and sent to Adminer.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"adminer-bridge/adminer"
	"adminer-bridge/config"
	"adminer-bridge/gate"
)

// Executor runs a batch in one database through Adminer. *adminer.Client
// implements it. database is never empty and always on the environment's
// allowlist.
type Executor interface {
	Exec(ctx context.Context, database, batch string, limit int) ([]adminer.Block, error)
}

// Result is a query result.
type Result struct {
	Columns     []string    `json:"columns"`
	Rows        [][]*string `json:"rows"`
	RowCount    int         `json:"row_count"`
	Truncated   bool        `json:"truncated"`
	ExecutedSQL string      `json:"executed_sql"`
	Database    string      `json:"database"` // the database that answered
}

// EnvInfo describes one environment and its allowed databases.
type EnvInfo struct {
	Name      string         `json:"name"`
	URL       string         `json:"url"`
	Schema    string         `json:"schema"`
	Database  string         `json:"database"`  // default database
	Databases []DatabaseInfo `json:"databases"` // allowed databases, in config order
}

// DatabaseInfo describes one allowed database and what was learned on its
// first connect.
type DatabaseInfo struct {
	Name               string   `json:"name"`
	Connected          bool     `json:"connected"`
	Superuser          *bool    `json:"superuser,omitempty"`
	Extensions         []string `json:"extensions,omitempty"`
	UntrustedLanguages []string `json:"untrusted_languages,omitempty"`
	Warnings           []string `json:"warnings,omitempty"`
}

const (
	DefaultMaxRows = 100
	HardMaxRows    = 1000
)

var riskyExtensions = []string{"dblink", "postgres_fdw", "adminpack", "pg_cron", "file_fdw"}

const (
	qFuncs = "SELECT DISTINCT p.proname FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'pg_catalog'"
	qSuper = "SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.rolname = current_user"
	qExts  = "SELECT e.extname FROM pg_catalog.pg_extension e ORDER BY e.extname"
	qLangs = "SELECT l.lanname FROM pg_catalog.pg_language l WHERE NOT l.lanpltrusted AND l.lanname NOT IN ('internal', 'c') ORDER BY l.lanname"
)

// envState serializes all queries of one environment, whatever the database:
// at most one query per environment is in flight.
type envState struct {
	env *config.Env
	ex  Executor
	mu  sync.Mutex
	dbs map[string]*dbState // one per allowed database; built once, never changed
}

// dbState holds what first connect learned about one database.
type dbState struct {
	name  string
	funcs gate.FuncSet                 // guarded by envState.mu
	info  atomic.Pointer[DatabaseInfo] // nil until first connect succeeds; set under envState.mu, read lock-free
}

// Bridge runs gated read-only queries against configured environments.
type Bridge struct {
	cfg  *config.Config
	envs map[string]*envState
}

// New builds a Bridge with real Adminer clients. No network traffic happens
// until the first Query.
func New(cfg *config.Config) (*Bridge, error) {
	ex := map[string]Executor{}
	for name, env := range cfg.Environments {
		c, err := adminer.NewClient(env)
		if err != nil {
			return nil, fmt.Errorf("environment %q: %w", name, err)
		}
		ex[name] = c
	}
	return NewWithExecutors(cfg, ex), nil
}

// NewWithExecutors builds a Bridge over the given executors (for tests).
func NewWithExecutors(cfg *config.Config, ex map[string]Executor) *Bridge {
	b := &Bridge{cfg: cfg, envs: map[string]*envState{}}
	for name, env := range cfg.Environments {
		st := &envState{env: env, ex: ex[name], dbs: map[string]*dbState{}}
		for _, db := range env.AllowedDatabases() {
			st.dbs[db] = &dbState{name: db}
		}
		b.envs[name] = st
	}
	return b
}

// Query validates sql, runs it read-only in database of env and returns at
// most maxRows rows. database "" is the environment's default database; any
// other value must be on the environment's allowlist. Gate rejections are
// returned as *gate.RejectError.
func (b *Bridge) Query(ctx context.Context, env, database, sql string, maxRows int) (*Result, error) {
	if maxRows <= 0 {
		maxRows = DefaultMaxRows
	}
	if maxRows > HardMaxRows {
		maxRows = HardMaxRows
	}
	res, err := b.query(ctx, env, database, sql, maxRows+1)
	if err != nil {
		return nil, err
	}
	if len(res.Rows) > maxRows {
		res.Rows = res.Rows[:maxRows]
		res.Truncated = true
	}
	res.RowCount = len(res.Rows)
	return res, nil
}

// lookup returns the state of env and of its database (resolved against the
// allowlist; "" is the default). Nothing is sent to Adminer.
func (b *Bridge) lookup(env, database string) (*envState, *dbState, error) {
	st, ok := b.envs[env]
	if !ok {
		return nil, nil, fmt.Errorf("unknown environment %q; valid environments: %s", env, strings.Join(b.cfg.Names(), ", "))
	}
	name, err := st.env.ResolveDatabase(database)
	if err != nil {
		return nil, nil, err
	}
	ds, ok := st.dbs[name]
	if !ok { // unreachable: dbs is built from the same allowlist
		return nil, nil, fmt.Errorf("environment %q: database %q has no state", env, name)
	}
	return st, ds, nil
}

// query is the single gated path shared by Query and the catalog helpers:
// env lookup, database resolution, per-env lock, per-database first-connect
// init, gate.Validate with that database's FuncSet, wrap, execute, echo
// check. limit is passed to Exec (0 = unlimited).
func (b *Bridge) query(ctx context.Context, env, database, sql string, limit int) (*Result, error) {
	st, ds, err := b.lookup(env, database)
	if err != nil {
		return nil, err
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	if ds.info.Load() == nil {
		if err := st.init(ctx, ds); err != nil {
			return nil, fmt.Errorf("environment %q, database %q: first connect failed: %w", env, ds.name, err)
		}
	}

	res, err := st.run(ctx, ds.name, sql, ds.funcs, limit)
	if err != nil {
		var rej *gate.RejectError
		if errors.As(err, &rej) {
			return nil, err
		}
		return nil, fmt.Errorf("environment %q, database %q: %w", env, ds.name, err)
	}
	res.RowCount = len(res.Rows)
	res.Database = ds.name
	return res, nil
}

// ListEnvironments describes every environment, sorted by name, with its
// allowed databases in config order. It takes no locks, so it never waits
// for a running query.
func (b *Bridge) ListEnvironments() []EnvInfo {
	var out []EnvInfo
	for _, name := range b.cfg.Names() {
		st := b.envs[name]
		e := EnvInfo{Name: name, URL: st.env.URL, Schema: st.env.Schema, Database: st.env.Database}
		for _, db := range st.env.AllowedDatabases() {
			if info := st.dbs[db].info.Load(); info != nil {
				e.Databases = append(e.Databases, *info)
			} else {
				e.Databases = append(e.Databases, DatabaseInfo{Name: db})
			}
		}
		out = append(out, e)
	}
	return out
}

func (st *envState) wrap(stmt string) string {
	return gate.Wrap(stmt, gate.Timeouts{Statement: st.env.StatementTimeout, Lock: st.env.LockTimeout})
}

// run validates (with fs), wraps, executes in database and checks one statement.
func (st *envState) run(ctx context.Context, database, sql string, fs gate.FuncSet, limit int) (*Result, error) {
	stmt, err := gate.Validate(sql, fs)
	if err != nil {
		return nil, err
	}
	batch := st.wrap(stmt)
	blocks, err := st.ex.Exec(ctx, database, batch, limit)
	if err != nil {
		return nil, err
	}
	res, err := checkBlocks(blocks, batch, stmt)
	if err != nil {
		return nil, err
	}
	res.ExecutedSQL = stmt
	return res, nil
}

// firstColumn returns the first column of every row (NULL cells are skipped).
func firstColumn(r *Result) []string {
	var out []string
	for _, row := range r.Rows {
		if len(row) > 0 && row[0] != nil {
			out = append(out, *row[0])
		}
	}
	return out
}

// init loads the function allow-list and role facts of database ds. Called
// with st.mu held; on failure ds.info stays nil so the next call retries.
// Extensions and languages differ per database, so each database has its own.
func (st *envState) init(ctx context.Context, ds *dbState) error {
	var empty gate.FuncSet
	step := func(q string) ([]string, error) {
		r, err := st.run(ctx, ds.name, q, empty, 0)
		if err != nil {
			return nil, err
		}
		return firstColumn(r), nil
	}
	procs, err := step(qFuncs)
	if err != nil {
		return err
	}
	super, err := step(qSuper)
	if err != nil {
		return err
	}
	exts, err := step(qExts)
	if err != nil {
		return err
	}
	langs, err := step(qLangs)
	if err != nil {
		return err
	}

	funcs := make(gate.FuncSet, len(procs))
	for _, p := range procs {
		funcs[p] = struct{}{}
	}
	info := &DatabaseInfo{Name: ds.name, Connected: true, Extensions: exts, UntrustedLanguages: langs}
	if len(super) > 0 {
		// Rendering of booleans depends on Adminer's PHP pg driver; accept
		// the common spellings and refuse to guess otherwise.
		switch super[0] {
		case "t", "true", "1":
			v := true
			info.Superuser = &v
			info.Warnings = append(info.Warnings, "login role is a superuser; a dedicated read-only role is recommended")
		case "f", "false", "0":
			v := false
			info.Superuser = &v
		default:
			info.Warnings = append(info.Warnings, fmt.Sprintf("could not determine whether login role is a superuser (got %s)", super[0]))
		}
	}
	for _, e := range exts {
		for _, r := range riskyExtensions {
			if e == r {
				info.Warnings = append(info.Warnings, "risky extension installed: "+e)
			}
		}
	}
	for _, l := range langs {
		info.Warnings = append(info.Warnings, "untrusted language installed: "+l)
	}
	for _, w := range info.Warnings {
		fmt.Fprintf(os.Stderr, "adminer-bridge: environment %q, database %q: warning: %s\n", st.env.Name, ds.name, w)
	}
	ds.funcs = funcs
	ds.info.Store(info)
	return nil
}

// checkBlocks verifies the response of a wrapped batch and extracts the
// result of the caller's statement (block gate.WrappedStmtIndex).
//
// Echo comparison: from adminer/testdata/ok.html and multiline.html, Adminer
// echoes each statement without its trailing ';' and with no whitespace
// changes (HTML entities are decoded by the parser). The deparsed statement
// has no ';' or line breaks (gate rejects them), so the echo must equal it
// exactly; a Truncated echo (Adminer shortens long statements) must be a
// prefix of it.
func checkBlocks(blocks []adminer.Block, batch, stmt string) (*Result, error) {
	// With error_stops, a failing wrapper statement ends the response early,
	// so look for failures before checking the block count.
	n := min(len(blocks), gate.WrappedStmtIndex)
	for _, bl := range blocks[:n] {
		if bl.Kind != adminer.KindMessage {
			return nil, fmt.Errorf("read-only wrapper failed: %s", bl.Message)
		}
	}
	if len(blocks) <= gate.WrappedStmtIndex {
		return nil, fmt.Errorf("unexpected Adminer response: got %d result blocks, need at least %d", len(blocks), gate.WrappedStmtIndex+1)
	}
	// The wrapper statements Adminer echoed must be the ones we sent
	// (confirms BEGIN TRANSACTION READ ONLY and the SET LOCALs ran).
	parts := strings.Split(batch, ";\n")
	for i := 0; i < gate.WrappedStmtIndex; i++ {
		if i >= len(parts) || blocks[i].SQL != parts[i] {
			return nil, errors.New("read-only wrapper failed: Adminer echoed an unexpected wrapper statement")
		}
	}
	b := blocks[gate.WrappedStmtIndex]
	if b.Truncated {
		if b.SQL == "" || !strings.HasPrefix(stmt, b.SQL) {
			return nil, errMismatch
		}
	} else if b.SQL != stmt {
		return nil, errMismatch
	}
	switch b.Kind {
	case adminer.KindError:
		return nil, errors.New(b.Message)
	case adminer.KindTable:
		return &Result{Columns: b.Columns, Rows: b.Rows}, nil
	default: // KindMessage: "No rows."
		return &Result{Columns: []string{}, Rows: [][]*string{}}, nil
	}
}

var errMismatch = errors.New("adminer executed a different statement than approved; aborting")
