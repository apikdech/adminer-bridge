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

	"adminer-bridge/adminer"
	"adminer-bridge/config"
	"adminer-bridge/gate"
)

// Executor runs a batch against Adminer. *adminer.Client implements it.
type Executor interface {
	Exec(ctx context.Context, batch string, limit int) ([]adminer.Block, error)
}

// Result is a query result.
type Result struct {
	Columns     []string    `json:"columns"`
	Rows        [][]*string `json:"rows"`
	RowCount    int         `json:"row_count"`
	Truncated   bool        `json:"truncated"`
	ExecutedSQL string      `json:"executed_sql"`
}

// EnvInfo describes one environment and what was learned on first connect.
type EnvInfo struct {
	Name               string   `json:"name"`
	URL                string   `json:"url"`
	Database           string   `json:"database"`
	Schema             string   `json:"schema"`
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

type envState struct {
	env   *config.Env
	ex    Executor
	mu    sync.Mutex
	funcs gate.FuncSet
	info  *EnvInfo // nil until first connect succeeds; guarded by mu
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
		b.envs[name] = &envState{env: env, ex: ex[name]}
	}
	return b
}

// Query validates sql, runs it read-only in env and returns at most maxRows rows.
// Gate rejections are returned as *gate.RejectError.
func (b *Bridge) Query(ctx context.Context, env, sql string, maxRows int) (*Result, error) {
	st, ok := b.envs[env]
	if !ok {
		return nil, fmt.Errorf("unknown environment %q; valid environments: %s", env, strings.Join(b.cfg.Names(), ", "))
	}
	if maxRows <= 0 {
		maxRows = DefaultMaxRows
	}
	if maxRows > HardMaxRows {
		maxRows = HardMaxRows
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	if st.info == nil {
		if err := st.init(ctx); err != nil {
			return nil, fmt.Errorf("environment %q: first connect failed: %w", env, err)
		}
	}

	stmt, err := gate.Validate(sql, st.funcs)
	if err != nil {
		return nil, err
	}
	blocks, err := st.ex.Exec(ctx, st.wrap(stmt), maxRows+1)
	if err != nil {
		return nil, fmt.Errorf("environment %q: %w", env, err)
	}
	res, err := checkBlocks(blocks, stmt)
	if err != nil {
		return nil, fmt.Errorf("environment %q: %w", env, err)
	}
	res.ExecutedSQL = stmt
	if len(res.Rows) > maxRows {
		res.Rows = res.Rows[:maxRows]
		res.Truncated = true
	}
	res.RowCount = len(res.Rows)
	return res, nil
}

// ListEnvironments describes every environment, sorted by name.
func (b *Bridge) ListEnvironments() []EnvInfo {
	var out []EnvInfo
	for _, name := range b.cfg.Names() {
		st := b.envs[name]
		st.mu.Lock()
		if st.info != nil {
			out = append(out, *st.info)
		} else {
			out = append(out, EnvInfo{Name: name, URL: st.env.URL, Database: st.env.Database, Schema: st.env.Schema})
		}
		st.mu.Unlock()
	}
	return out
}

func (st *envState) wrap(stmt string) string {
	return gate.Wrap(stmt, gate.Timeouts{Statement: st.env.StatementTimeout, Lock: st.env.LockTimeout})
}

// run validates (with fs), wraps, executes and checks one statement.
func (st *envState) run(ctx context.Context, sql string, fs gate.FuncSet, limit int) (*Result, error) {
	stmt, err := gate.Validate(sql, fs)
	if err != nil {
		return nil, err
	}
	blocks, err := st.ex.Exec(ctx, st.wrap(stmt), limit)
	if err != nil {
		return nil, err
	}
	return checkBlocks(blocks, stmt)
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

// init loads the function allow-list and role facts. Called with st.mu held;
// on failure st.info stays nil so the next call retries.
func (st *envState) init(ctx context.Context) error {
	var empty gate.FuncSet
	step := func(q string) ([]string, error) {
		r, err := st.run(ctx, q, empty, 0)
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
	info := &EnvInfo{
		Name: st.env.Name, URL: st.env.URL, Database: st.env.Database, Schema: st.env.Schema,
		Connected: true, Extensions: exts, UntrustedLanguages: langs,
	}
	if len(super) > 0 {
		v := super[0] == "t"
		info.Superuser = &v
		if v {
			info.Warnings = append(info.Warnings, "login role is a superuser; a dedicated read-only role is recommended")
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
		fmt.Fprintf(os.Stderr, "adminer-bridge: environment %q: warning: %s\n", st.env.Name, w)
	}
	st.funcs, st.info = funcs, info
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
func checkBlocks(blocks []adminer.Block, stmt string) (*Result, error) {
	if len(blocks) <= gate.WrappedStmtIndex {
		return nil, fmt.Errorf("unexpected Adminer response: got %d result blocks, need at least %d", len(blocks), gate.WrappedStmtIndex+1)
	}
	for _, bl := range blocks[:gate.WrappedStmtIndex] {
		if bl.Kind != adminer.KindMessage {
			return nil, fmt.Errorf("read-only wrapper failed: %s", bl.Message)
		}
	}
	b := blocks[gate.WrappedStmtIndex]
	if b.Truncated {
		if !strings.HasPrefix(stmt, b.SQL) {
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
