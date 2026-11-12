# Adminer Bridge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Go MCP server (plus a small CLI) that lets an LLM run deterministic, read-only-gated SQL against Postgres through an Adminer web UI over plain HTTP.

**Architecture:** `gate` (pure SQL validation via Postgres's own parser, deparse, read-only wrapper) → `adminer` (HTTP client: login, CSRF token, run batch, parse HTML into blocks) → `bridge` (per-environment orchestration, built-in function set, role check, catalog helpers; the package a future bot imports) → `mcpserver` + `cmd/adminer-bridge` (thin MCP stdio server and CLI).

**Tech Stack:** Go 1.27, `github.com/pganalyze/pg_query_go/v6` (cgo), `github.com/modelcontextprotocol/go-sdk/mcp`, `github.com/PuerkitoBio/goquery`, `gopkg.in/yaml.v3`. Use `@latest` of each; check current API of go-sdk with context7 before Task 8.

**Spec:** `docs/superpowers/specs/2026-11-12-adminer-bridge-design.md` — read it before starting any task.

## Global Constraints

- Repo root: `~/Other/ok/adminer-bridge`. Module path: `adminer-bridge`.
- Postgres + Adminer `pgsql` driver only. Local test env: Adminer 5.4.1 at `http://localhost:8080/`, server `database`, user `postgres`, database `glchat`, schema `public`, Postgres 18. Password comes from env var `ADMINER_LOCAL_PASSWORD` (local value: `docker inspect glchat-be-database-1` → `POSTGRES_PASSWORD`; never commit it).
- Nothing reaches Adminer without passing `gate.Validate` — including the bridge's own catalog queries. No bypass path in production code.
- The text sent to Adminer is the **deparsed** statement, never the caller's raw text.
- Defaults: `statement_timeout` 15s, `lock_timeout` 2s, `idle_in_transaction_session_timeout` = statement timeout, HTTP timeout 30s, `max_rows` default 100 / hard max 1000.
- At most one query in flight per environment.
- Passwords and cookies never appear in tool output, errors, or logs.
- Config default path `~/.config/adminer-bridge/config.yaml`, overridden by `--config` flag or `ADMINER_BRIDGE_CONFIG` env var. Passwords only via env var named in config (`password_env`).
- **Never retry a failed login** (Adminer throttles IPs after failed logins). Retry at most once, and only for expired session or invalid CSRF token.
- When sending forms with curl for fixtures/manual tests, use `--form-string`, never `-F` (`-F` treats `;` as an attribute separator and silently truncates the batch).

## Review Focus

1. **Adminer session expires mid-use** (idle timeout, Adminer container restarted) → the next query transparently re-logs in and succeeds once; a second login page response is an error. Test in Task 6.
2. **Wrong password / Adminer down / login throttled** → one clear error naming the environment and Adminer's message, no retry, no password in the error. Test in Task 6.
3. **Zero-row results and value fidelity**: empty result, empty string vs NULL vs `'NULL'`, values containing `<`, `&`, quotes, newlines, unicode → exact round trip, empty rows not an error. Test in Task 5.
4. **Adminer splits the batch differently from what the gate approved** (e.g. a literal containing `;` or a newline) → rejected before sending; and after sending, the echoed statement text must match the deparsed statement or the bridge reports an error. Tests in Tasks 2 and 7.
5. **Large results** → capped at `max_rows` with `truncated: true`, requests over 1000 clamped, internal catalog loads (≈3000 rows) unlimited. Test in Task 7.

---

## File Structure

```
go.mod
cmd/adminer-bridge/main.go         # subcommands: serve (MCP stdio), query (CLI)
config/config.go, config_test.go   # YAML config, env resolution, defaults
gate/gate.go                       # Validate, RejectError, rules, tree walk
gate/funcs.go                      # blocklist + FuncSet
gate/wrap.go                       # Timeouts, Wrap (layer 3 batch text)
gate/gate_test.go                  # structural rules + bypass corpus
gate/funcs_test.go                 # function rules
adminer/parse.go, parse_test.go    # HTML → []Block
adminer/client.go, client_test.go  # HTTP client (login, token, exec, retry)
adminer/testdata/*.html            # fixtures captured from local Adminer 5.4.1
bridge/bridge.go, bridge_test.go   # Bridge, Query, env state, role check
bridge/catalog.go, catalog_test.go # ListTables, DescribeTable
mcpserver/server.go, server_test.go
integration/integration_test.go    # opt-in, against local Adminer
README.md
```

---

### Task 1: Module scaffold + config

**Files:**
- Create: `go.mod`, `config/config.go`, `config/config_test.go`, `.gitignore` (ignore `/adminer-bridge` binary)

**Interfaces:**
- Produces:
  ```go
  package config
  type Env struct {
      Name             string        // map key
      URL              string        `yaml:"url"`        // Adminer base URL, e.g. http://localhost:8080/
      Server           string        `yaml:"server"`
      Username         string        `yaml:"username"`
      Database         string        `yaml:"database"`
      Schema           string        `yaml:"schema"`        // default "public"
      PasswordEnv      string        `yaml:"password_env"`
      StatementTimeout time.Duration `yaml:"statement_timeout"` // default 15s
      LockTimeout      time.Duration `yaml:"lock_timeout"`      // default 2s
      HTTPTimeout      time.Duration `yaml:"http_timeout"`      // default 30s
  }
  type Config struct { Environments map[string]*Env `yaml:"environments"` }
  func Load(path string) (*Config, error)       // parse + defaults + validate
  func DefaultPath() string                      // ADMINER_BRIDGE_CONFIG or ~/.config/adminer-bridge/config.yaml
  func (e *Env) Password() (string, error)       // os.Getenv(PasswordEnv); error if unset/empty, names the env var only
  func (c *Config) Names() []string              // sorted
  ```

- [ ] **Step 1:** `go mod init adminer-bridge`; add `gopkg.in/yaml.v3`.
- [ ] **Step 2: Write failing tests** in `config/config_test.go`:
  - `TestLoadAppliesDefaults`: YAML with one env `local` lacking schema/timeouts → `Schema=="public"`, `StatementTimeout==15*time.Second`, `LockTimeout==2*time.Second`, `HTTPTimeout==30*time.Second`, `Name=="local"`.
  - `TestLoadRejectsMissingFields`: env missing `url`, `server`, `username`, `database`, or `password_env` → error mentioning env name and field.
  - `TestLoadRejectsInlinePassword`: YAML with a `password:` key → error (use `yaml` `KnownFields(true)` decoding).
  - `TestPasswordFromEnv`: `t.Setenv("X_PW","s3cret")` → `"s3cret"`; unset → error containing `X_PW` and not containing any value.
  - `TestDefaultPathEnvOverride`: `t.Setenv("ADMINER_BRIDGE_CONFIG","/x.yaml")` → `/x.yaml`.
- [ ] **Step 3:** Run `go test ./config/` → FAIL (undefined).
- [ ] **Step 4:** Implement in `config/config.go`.
- [ ] **Step 5:** Run `go test ./config/` → PASS.
- [ ] **Step 6:** Commit `feat: config loading`.

---

### Task 2: Gate — parser rules, deparse, splitter safety, wrapper

**Files:**
- Create: `gate/gate.go`, `gate/wrap.go`, `gate/gate_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  ```go
  package gate
  type Rule string
  const (
      RuleParse           Rule = "parse_error"
      RuleSingleStatement Rule = "single_statement"
      RuleStatementType   Rule = "statement_type"     // top level not SELECT / EXPLAIN
      RuleExplainAnalyze  Rule = "explain_analyze"
      RuleNestedStatement Rule = "nested_statement"   // any *Stmt other than SelectStmt below top level (data-modifying CTEs etc.)
      RuleSelectInto      Rule = "select_into"
      RuleLocking         Rule = "locking_clause"
      RuleFunction        Rule = "function_not_allowed" // Task 3
      RuleUnsafeChars     Rule = "unsafe_characters"  // ';' '\n' '\r' in deparsed text
      RuleDeparse         Rule = "deparse_error"
  )
  type RejectError struct { Rule Rule; Detail string }
  func (e *RejectError) Error() string // "rejected by read-only gate [<rule>]: <detail>"
  type FuncSet map[string]struct{}     // built-in pg_catalog function names; nil/empty => every function call rejected
  func Validate(sql string, funcs FuncSet) (string, error) // returns deparsed statement

  type Timeouts struct { Statement, Lock time.Duration }
  const WrappedStmtIndex = 4 // index of the caller's statement among the batch's statements
  func Wrap(stmt string, t Timeouts) string
  ```

**Approach (algorithm the tests don't fully determine):**
1. `pg_query.Parse(sql)`; error → `RuleParse`. `len(Stmts) != 1` → `RuleSingleStatement`.
2. `pg_query.ParseToJSON(sql)`, unmarshal into `any`, walk recursively. The top statement node is the single key under `stmts[0].stmt`: must be `SelectStmt` or `ExplainStmt`, else `RuleStatementType`. For `ExplainStmt`: any `DefElem` in `options` with `defname == "analyze"` → `RuleExplainAnalyze`.
3. Walk every map in the tree (below the top statement): a key ending in `Stmt` other than `SelectStmt` → `RuleNestedStatement`; key `intoClause` → `RuleSelectInto`; key `lockingClause` → `RuleLocking`; key `FuncCall` → function check (Task 3; in this task, reject every `FuncCall` with `RuleFunction` when `funcs` is empty, which is the only case Task 2 tests).
4. `pg_query.Deparse(tree)`; error → `RuleDeparse`. Deparsed text containing `;`, `\n` or `\r` → `RuleUnsafeChars`, detail suggests `chr(59)` / `chr(10)`. (This guarantees Adminer's splitter, which also honours line-based `DELIMITER`, sees exactly one statement.)
- `Wrap` returns these five statements joined by `;\n`, with a trailing `;`, timeouts formatted as `'<n>ms'`:
  `BEGIN TRANSACTION READ ONLY` / `SET LOCAL statement_timeout = '<S>ms'` / `SET LOCAL lock_timeout = '<L>ms'` / `SET LOCAL idle_in_transaction_session_timeout = '<S>ms'` / `<stmt>` / `ROLLBACK` — i.e. stmt at index 4, `ROLLBACK` at index 5.

- [ ] **Step 1: Write failing tests** in `gate/gate_test.go`, table-driven, asserting `errors.As(err, &re)` and `re.Rule`:
  - Must pass (nil error, `funcs=nil`): `select * from users`, `select u.id from users u join orgs o on o.id = u.org_id where u.id = 1`, `with x as (select 1 as a) select a from x`, `select * from (select 1) s`, `explain select * from users`, `explain (format json) select 1`, `select id from users order by id limit 5 offset 2`.
  - Window function / aggregate cases move to Task 3 (they are function calls).
  - Rejections (input → rule):
    - `select 1; select 2` → single_statement; `select 1; drop table users` → single_statement; `select 1 /* ; */ ; delete from users` → single_statement; `select 1 -- x\n; delete from users` → single_statement; `commit; delete from users` → single_statement
    - `delete from users`, `update users set a=1`, `insert into users values (1)`, `merge into t using s on true when matched then delete`, `drop table users`, `truncate users`, `copy users to stdout`, `set role postgres`, `reset all`, `do $$ begin delete from users; end $$`, `call p()`, `prepare p as select 1`, `execute p`, `listen x`, `notify x`, `vacuum users`, `analyze users`, `begin`, `commit`, `rollback`, `lock table users`, `create table t (i int)`, `grant select on users to public`, `refresh materialized view mv`, `discard all`, `checkpoint`, `load 'x'`, `show all` → statement_type
    - `with d as (delete from users returning *) select * from d`, `with u as (update users set a=1 returning *) select 1`, `with i as (insert into t values (1) returning *) select 1` → nested_statement
    - `select * into newtab from users` → select_into
    - `select * from users for update`, `... for share`, `... for no key update`, `... for key share`, `select * from users for update skip locked` → locking
    - `explain analyze delete from users`, `explain analyze select 1`, `explain (analyze) select 1`, `explain (analyze false) select 1`, `explain (ANALYZE, buffers) select 1` → explain_analyze
    - `explain delete from users` → nested_statement (explained statement is not a SELECT)
    - `select 'a;b' as x` → unsafe_characters; `select $$a\nb$$` → unsafe_characters; `select E'a\nb'` → unsafe_characters; `select 1 as "we;ird"` → unsafe_characters
    - `selec 1`, `` (empty), `   ` → parse_error or single_statement (assert non-nil error, any rule)
    - `select U&"\0064elete"` style unicode identifiers must not panic (assert returns without panic; any outcome)
  - `TestValidateReturnsDeparsed`: `SELECT   *   FROM users WHERE id=1` → `SELECT * FROM users WHERE id = 1`.
  - `TestRejectErrorMessage`: `Error()` has prefix `rejected by read-only gate [statement_type]:`.
  - `TestWrap`: `Wrap("SELECT 1", Timeouts{15*time.Second, 2*time.Second})` equals exactly
    `"BEGIN TRANSACTION READ ONLY;\nSET LOCAL statement_timeout = '15000ms';\nSET LOCAL lock_timeout = '2000ms';\nSET LOCAL idle_in_transaction_session_timeout = '15000ms';\nSELECT 1;\nROLLBACK;"`
- [ ] **Step 2:** `go get github.com/pganalyze/pg_query_go/v6@latest`; run `go test ./gate/` → FAIL. (First cgo build of pg_query takes a few minutes.)
- [ ] **Step 3:** Implement `gate/gate.go`, `gate/wrap.go` per the approach.
- [ ] **Step 4:** Run `go test ./gate/` → PASS. If the JSON tree shape differs from the approach (e.g. a `RawStmt` wrapper), adapt the walker, not the tests.
- [ ] **Step 5:** Commit `feat(gate): parser-based read-only rules and wrapper`.

---

### Task 3: Gate — function allowlist + blocklist

**Files:**
- Create: `gate/funcs.go`, `gate/funcs_test.go`
- Modify: `gate/gate.go` (FuncCall check)

**Interfaces:**
- Consumes: `FuncSet`, `RuleFunction`, `Validate` from Task 2.
- Produces: `func Blocked(name string) bool` (lower-cased name check), exported for the bridge's tests.

**Rules for a `FuncCall`** (`funcname` is a list of `String{sval}` nodes):
- More than 2 parts, or 2 parts with schema ≠ `pg_catalog` → reject.
- Name (last part) lower-cased hits the blocklist → reject, even if in `funcs`.
- Name (exact, case-sensitive) not in `funcs` → reject. Detail names the function.

**Blocklist** (`gate/funcs.go`):
- Exact: `pg_terminate_backend`, `pg_cancel_backend`, `pg_reload_conf`, `pg_rotate_logfile`, `pg_switch_wal`, `pg_promote`, `pg_log_backend_memory_contexts`, `pg_log_standby_snapshot`, `set_config`, `pg_sleep`, `pg_sleep_for`, `pg_sleep_until`, `pg_read_file`, `pg_read_binary_file`, `pg_stat_file`, `pg_notify`, `nextval`, `setval`, `pg_backup_start`, `pg_backup_stop`, `pg_import_system_collations`, `pg_sync_replication_slots`
- Prefixes: `pg_advisory_`, `pg_try_advisory_`, `pg_ls_`, `lo_`, `dblink`, `pg_logical_`, `pg_create_`, `pg_drop_`, `pg_replication_origin_`, `pg_stat_reset`, `pg_wal_replay_`, `pg_file_`
- Contains: `replication_slot`
- Suffixes: `_to_xml`, `_to_xmlschema`, `_to_xml_and_xmlschema`

- [ ] **Step 1: Write failing tests** in `gate/funcs_test.go` with `funcs := FuncSet{"count":{}, "lower":{}, "row_number":{}, "now":{}, "pg_sleep":{}, "query_to_xml":{}, "set_config":{}, "pg_terminate_backend":{}, "lo_import":{}, "pg_advisory_lock":{}, "pg_ls_dir":{}, "pg_read_file":{}, "table_to_xml":{}, "nextval":{}, "pg_stat_reset":{}}`:
  - Pass: `select count(*) from users`, `select lower(name) from users`, `select row_number() over (order by id) from users`, `select pg_catalog.lower('A')`, `select now()`, `select * from users where id in (select 1)`.
  - Reject `function_not_allowed`: every blocklisted entry in the set above called plainly (`select pg_sleep(1)`), schema-qualified (`select pg_catalog.pg_sleep(1)`), upper-case (`select PG_SLEEP(1)`); `select "PG_SLEEP"(1)` (quoted → not in set); `select query_to_xml('delete from users', true, false, '')`; `select dblink('x','delete from users')` (not in set + blocklist); `select public.lower('a')`; `select x.pg_catalog.lower('a')`; `select my_func()`; function in FROM `select * from pg_sleep(1)`; function inside subquery, CTE, `WHERE`, `ORDER BY`, `CASE`, and `explain select pg_sleep(1)`.
  - `TestNilFuncSetRejectsAllCalls`: `Validate("select count(*) from users", nil)` → function_not_allowed.
  - `TestBlocked`: table of names → true/false, including `pg_advisory_xact_lock` true, `pg_try_advisory_lock` true, `database_to_xml` true, `cursor_to_xmlschema` true, `pg_create_restore_point` true, `pg_drop_replication_slot` true, `lo_unlink` true, `lower` false, `pg_get_constraintdef` false, `count` false.
- [ ] **Step 2:** `go test ./gate/` → FAIL.
- [ ] **Step 3:** Implement `funcs.go` and wire the FuncCall check into the walker.
- [ ] **Step 4:** `go test ./gate/` → PASS (Task 2 tests still pass).
- [ ] **Step 5:** Commit `feat(gate): built-in function allowlist and blocklist`.

---

### Task 4: Capture Adminer HTML fixtures

**Files:**
- Create: `adminer/testdata/*.html`, `scripts/capture-fixtures.sh`

The parser and client tests (Tasks 5–6) run against real Adminer 5.4.1 output. Capture with a script (curl, cookie jar, `--form-string`; reads password from `ADMINER_LOCAL_PASSWORD`). POST the SQL page `http://localhost:8080/?pgsql=database&username=postgres&db=glchat&ns=public&sql=` with form fields `query`, `limit`, `error_stops=1`, `token` (token from `input[name=token]` inside `form#form` of the previously fetched page). Login is a POST to `http://localhost:8080/` with `auth[driver]=pgsql`, `auth[server]`, `auth[username]`, `auth[password]`, `auth[db]` → 302.

Strip the session cookie values and nonces only if they appear verbatim in a fixture; otherwise store as-is.

| Fixture | How |
|---|---|
| `login.html` | GET `/` with no cookies |
| `login_failed.html` | login POST with a wrong password **once** |
| `sql_page.html` | GET SQL page after login |
| `ok.html` | `Wrap("SELECT 1 AS a, NULL AS b, 'NULL' AS c, '' AS d, '<b>&\"' AS e, 'é漢' AS f", 15s/2s)` (write the batch literally) |
| `multiline.html` | wrapped `SELECT E'line1\nline2' AS m, '  padded  ' AS p` (newline values bypass the gate here on purpose — fixture only) |
| `empty.html` | wrapped `SELECT 1 AS a WHERE false` |
| `limit.html` | wrapped `SELECT g FROM generate_series(1,5) g` with `limit=3` |
| `explain.html` | wrapped `EXPLAIN SELECT 1` |
| `error_stmt.html` | wrapped `SELECT * FROM no_such_table` |
| `error_readonly.html` | wrapped `CREATE TABLE zz_probe(i int)` |
| `long_stmt.html` | wrapped SELECT whose text is > 1100 chars (e.g. many `1 AS cN` columns) — records whether Adminer shortens the echoed `<pre>` |
| `bad_token.html` | POST with `token=1:2` |

- [ ] **Step 1:** Write `scripts/capture-fixtures.sh`, run it, confirm every file exists and is non-empty.
- [ ] **Step 2:** Open `empty.html`, `limit.html`, `multiline.html`, `long_stmt.html`, `bad_token.html` and write down (in a comment at the top of `adminer/parse.go` later) how Adminer renders: zero rows, the limit, newlines in values, long echoed statements (truncation marker if any), and the CSRF error text.
- [ ] **Step 3:** Commit `test(adminer): capture Adminer 5.4.1 fixtures`. Check `git diff --cached` contains no password.

---

### Task 5: Adminer HTML parser

**Files:**
- Create: `adminer/parse.go`, `adminer/parse_test.go`

**Interfaces:**
- Produces:
  ```go
  package adminer
  type BlockKind int
  const ( KindMessage BlockKind = iota; KindTable; KindError )
  type Block struct {
      SQL       string      // echoed statement text from <pre id="sql-N">, entity-decoded, trimmed
      Truncated bool        // true if Adminer shortened the echoed SQL (per Task 4 findings)
      Kind      BlockKind
      Message   string      // KindMessage / KindError text
      Columns   []string    // KindTable
      Rows      [][]*string // KindTable; nil = SQL NULL
  }
  type PageKind int
  const ( PageSQL PageKind = iota; PageLogin )
  func ParsePage(html []byte) (kind PageKind, blocks []Block, token string, pageErr string, err error)
  ```
  `pageErr` = text of a page-level `p.error` that is not attached to a statement (login failure, invalid CSRF token, throttling). `token` = `form#form input[name=token]` (or the login form's, if any).

**Approach:** with goquery, walk the direct children of `#content` in order. `pre[id^=sql-]` opens a new block. Until the next `pre`, the first of `div.scrollable > table` (ignoring anything inside `div.explain`), `p.message`, `p.error` sets the kind. A trailing `p.error` of the form `Error in query: <a>N</a>` is summary noise — ignore it. Table cell: if the `td` has exactly one child element `<i>` with text `NULL` and no other text → nil; otherwise the cell's text with `<br>` converted to `\n` as per Task 4 findings. Login page detection: presence of `input[name="auth[password]"]`.

- [ ] **Step 1: Write failing tests** against the fixtures:
  - `ok.html` → 6 blocks; blocks 0–3 and 5 `KindMessage`; block 4 `KindTable`, `Columns == [a b c d e f]`, row 0: `"1"`, nil, `"NULL"`, `""`, `"<b>&\""`, `"é漢"`; block 4 `SQL` begins with `SELECT 1 AS a`.
  - `ok.html`: no block comes from the `div.explain` table (no block with column `QUERY PLAN`).
  - `multiline.html` → `"line1\nline2"`, `"  padded  "` (assert exact; if Adminer cannot preserve padding, record that and assert what it gives).
  - `empty.html` → block 4 has zero rows and is not an error (Kind per Task 4 finding; `bridge` must treat both table-with-no-rows and a "no rows" message as empty).
  - `explain.html` → block 4 table with column `QUERY PLAN`.
  - `error_stmt.html` → block 4 `KindError`, `Message` contains `no_such_table`; exactly 5 blocks (stop on error).
  - `error_readonly.html` → block 4 `KindError` containing `read-only transaction`.
  - `long_stmt.html` → `Truncated` matches Task 4 finding.
  - `login.html`, `login_failed.html` → `PageLogin`; `login_failed.html` `pageErr` non-empty.
  - `bad_token.html` → `pageErr` contains the CSRF text from Task 4.
  - `sql_page.html` → `PageSQL`, `token` non-empty, zero blocks.
- [ ] **Step 2:** `go get github.com/PuerkitoBio/goquery@latest`; `go test ./adminer/` → FAIL.
- [ ] **Step 3:** Implement `adminer/parse.go`.
- [ ] **Step 4:** `go test ./adminer/` → PASS.
- [ ] **Step 5:** Commit `feat(adminer): parse SQL command result pages`.

---

### Task 6: Adminer HTTP client

**Files:**
- Create: `adminer/client.go`, `adminer/client_test.go`

**Interfaces:**
- Consumes: `config.Env`, `ParsePage`, `Block`.
- Produces:
  ```go
  type Client struct { /* env, http.Client with cookiejar, token string */ }
  func NewClient(env *config.Env) (*Client, error)
  func (c *Client) Exec(ctx context.Context, batch string, limit int) ([]Block, error) // limit 0 = unlimited
  var ErrLogin = errors.New("adminer login failed") // wrapped with env name + Adminer's message
  ```
  SQL page URL: `<URL>?pgsql=<server>&username=<user>&db=<database>&ns=<schema>&sql=` (query-escaped). Form: multipart, fields `query`, `limit` (empty string when 0), `error_stops=1`, `token`.

**Behaviour:**
- No token yet → GET SQL page; if login page → login, then GET SQL page again for the token.
- Login: POST `auth[driver]=pgsql`, `auth[server]`, `auth[username]`, `auth[password]`, `auth[db]` to `<URL>`; follow redirect. Still login page → `ErrLogin` with Adminer's `pageErr`, **no retry**.
- After POSTing the batch: response is login page (session expired) → login once and resend once; `pageErr` is the CSRF error → refetch token and resend once. A second failure of either kind → error. Every response updates the cached token.
- Errors include the env name; never the password or cookie values. HTTP timeout from `env.HTTPTimeout`.

- [ ] **Step 1: Write failing tests** with `httptest.Server` serving fixtures and recording requests:
  - `TestExecLogsInThenRuns`: server returns `login.html` until a login POST, then `sql_page.html` on GET and `ok.html` on POST → 6 blocks; exactly one login POST; batch POST carried the token from `sql_page.html` and `error_stops=1`; `query` field equals the batch byte-for-byte (including `;`).
  - `TestExecReloginOnceOnExpiredSession`: first batch POST returns `login.html`, then `ok.html` → success, two login POSTs total.
  - `TestExecFailsAfterSecondLoginPage`: batch POST always returns `login.html` → error, at most two login POSTs.
  - `TestLoginFailureNoRetry`: login POST always returns `login_failed.html` → `errors.Is(err, ErrLogin)`, exactly one login POST, error text contains env name and does not contain the password.
  - `TestExecRetriesOnceOnBadToken`: first batch POST returns `bad_token.html` → token refetched, resent, success.
  - `TestLimitField`: `Exec(..., 101)` sends `limit=101`; `Exec(..., 0)` sends `limit=`.
  - `TestUnreachable`: server closed → error containing env name.
- [ ] **Step 2:** `go test ./adminer/` → FAIL.
- [ ] **Step 3:** Implement `adminer/client.go`.
- [ ] **Step 4:** `go test ./adminer/` → PASS.
- [ ] **Step 5:** Commit `feat(adminer): HTTP client with session and token handling`.

---

### Task 7: Bridge — query path, env state, role check

**Files:**
- Create: `bridge/bridge.go`, `bridge/bridge_test.go`

**Interfaces:**
- Consumes: `config.Config`, `gate.Validate/Wrap/WrappedStmtIndex/FuncSet/RejectError`, `adminer.Block`, `adminer.NewClient`.
- Produces:
  ```go
  package bridge
  type Executor interface { Exec(ctx context.Context, batch string, limit int) ([]adminer.Block, error) }
  type Result struct {
      Columns     []string    `json:"columns"`
      Rows        [][]*string `json:"rows"`
      RowCount    int         `json:"row_count"`
      Truncated   bool        `json:"truncated"`
      ExecutedSQL string      `json:"executed_sql"`
  }
  type EnvInfo struct {
      Name, URL, Database, Schema string
      Connected           bool     `json:"connected"`
      Superuser           *bool    `json:"superuser,omitempty"`
      Extensions          []string `json:"extensions,omitempty"`
      UntrustedLanguages  []string `json:"untrusted_languages,omitempty"`
      Warnings            []string `json:"warnings,omitempty"`
  }
  const DefaultMaxRows, HardMaxRows = 100, 1000
  type Bridge struct { /* per-env: *config.Env, Executor, sync.Mutex, FuncSet, *EnvInfo */ }
  func New(cfg *config.Config) (*Bridge, error)                                  // real adminer clients
  func NewWithExecutors(cfg *config.Config, ex map[string]Executor) *Bridge      // tests
  func (b *Bridge) Query(ctx context.Context, env, sql string, maxRows int) (*Result, error)
  func (b *Bridge) ListEnvironments() []EnvInfo
  ```

**Behaviour:**
- Unknown env → error listing valid names.
- `maxRows <= 0` → 100; `> 1000` → 1000. Send `limit = maxRows+1`; if more than `maxRows` rows come back, trim and set `Truncated`.
- Per-env mutex held for the whole call (including first-connect init).
- **First connect** (once per env, under the mutex, retried on next call if it fails): run, each through `Validate` + `Wrap` with an empty FuncSet and limit 0:
  - `SELECT DISTINCT p.proname FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'pg_catalog'` → FuncSet
  - `SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.rolname = current_user`
  - `SELECT e.extname FROM pg_catalog.pg_extension e ORDER BY e.extname`
  - `SELECT l.lanname FROM pg_catalog.pg_language l WHERE NOT l.lanpltrusted AND l.lanname NOT IN ('internal', 'c') ORDER BY l.lanname`
  Warnings: superuser → `"login role is a superuser; a dedicated read-only role is recommended"`; any of `dblink`, `postgres_fdw`, `adminpack`, `pg_cron`, `file_fdw` in extensions → `"risky extension installed: <name>"`; each untrusted language → `"untrusted language installed: <name>"`. Log warnings to stderr once.
- Response check (`checkBlocks`): need ≥ 5 blocks; blocks 0–3 must be `KindMessage` (else error `"read-only wrapper failed: <message>"`); block 4's `SQL` must equal the deparsed statement (or be a prefix of it when `Truncated`) else error `"adminer executed a different statement than approved; aborting"`; block 4 `KindError` → error with Postgres's message; `KindTable` → result; `KindMessage` (zero rows) → empty result with columns empty.
- `Result.ExecutedSQL` = deparsed statement.
- Gate rejections are returned as-is (`*gate.RejectError`) so callers can show the rule.

- [ ] **Step 1: Write failing tests** with a fake `Executor` that records batches and returns scripted blocks:
  - `TestQueryRejectedNeverReachesExecutor`: `delete from users` → `*gate.RejectError`, fake recorded zero batches **after** init.
  - `TestFirstConnectLoadsFuncSetOnce`: fake returns `count` in the proname result; two `select count(*) from users` calls succeed; init queries sent exactly once.
  - `TestFuncRulesUseLoadedSet`: loaded set `{count, pg_sleep}` → `select count(*) from users` passes; `select pg_sleep(1)` rejected (blocklist) and `select my_func()` rejected (not in set), both `function_not_allowed`.
  - `TestQuerySendsWrappedDeparsed`: batch == `gate.Wrap("SELECT * FROM users WHERE id = 1", env timeouts)`; `limit` == 101 for default.
  - `TestTruncation`: fake returns 101 rows, maxRows default → 100 rows, `Truncated`; `maxRows=5000` → limit sent 1001.
  - `TestWrapperFailure`: block 1 is `KindError` → error contains `read-only wrapper failed`.
  - `TestEchoMismatch`: block 4 `SQL` = `"DELETE FROM users"` → error contains `different statement`.
  - `TestStatementError`: block 4 `KindError` "relation \"x\" does not exist" → error contains it.
  - `TestEmptyResult`: block 4 `KindMessage` → `RowCount 0`, no error.
  - `TestRoleWarnings`: init results superuser `t`, extensions `[dblink plpgsql]`, languages `[plpython3u]` → `ListEnvironments()` has 3 warnings and `Connected`.
  - `TestUnknownEnv`: error names valid envs.
  - `TestSerializedPerEnv`: two concurrent `Query` on one env → fake observes no overlap (fake sleeps and tracks in-flight count).
- [ ] **Step 2:** `go test ./bridge/` → FAIL.
- [ ] **Step 3:** Implement `bridge/bridge.go`.
- [ ] **Step 4:** `go test ./bridge/` → PASS.
- [ ] **Step 5:** Commit `feat(bridge): gated query path with per-env state`.

---

### Task 8: Bridge — catalog helpers

**Files:**
- Create: `bridge/catalog.go`, `bridge/catalog_test.go`

**Interfaces:**
- Consumes: `Bridge.Query` internals (same gated path; limit unlimited — call an unexported `query(ctx, env, sql, limit)` shared with `Query`).
- Produces:
  ```go
  func (b *Bridge) ListTables(ctx context.Context, env, schema string) (*Result, error)    // schema "" => env.Schema
  func (b *Bridge) DescribeTable(ctx context.Context, env, table string) (*TableDescription, error)
  type TableDescription struct {
      Schema, Table string
      Columns     *Result `json:"columns"`      // column_name, data_type, is_nullable, column_default
      Indexes     *Result `json:"indexes"`      // indexname, indexdef
      ForeignKeys *Result `json:"foreign_keys"` // conname, definition
  }
  func quoteLiteral(s string) string // '...' with ' doubled
  ```
- `table` may be `schema.table`; otherwise env schema. Names are only ever inserted via `quoteLiteral` and the resulting SQL still goes through the gate. Queries: `information_schema.tables` (BASE TABLE + VIEW, ordered), `information_schema.columns` ordered by `ordinal_position`, `pg_catalog.pg_indexes`, `pg_catalog.pg_constraint` with `pg_catalog.pg_get_constraintdef(c.oid)` where `contype = 'f'` joined via `pg_class`/`pg_namespace`. Unknown table → error `table <schema>.<table> not found`.

- [ ] **Step 1: Write failing tests** with the fake executor:
  - `TestListTablesUsesEnvSchema`: deparsed statement in the batch contains `'public'`.
  - `TestDescribeTableQuotesName`: `DescribeTable(ctx,"local","users'; drop")` → `*gate.RejectError` with rule `unsafe_characters` (the `;` stays inside the quoted literal and is caught after deparse); no batch is executed beyond init. `DescribeTable(ctx,"local","o'brien")` → the batch contains `'o''brien'`. (Pins: names cannot escape the literal; worst case is a rejection.)
  - `TestDescribeTableSchemaQualified`: `"auth.users"` → batch filters schema `'auth'` and table `'users'`.
  - `TestDescribeTableNotFound`: columns result empty → error contains `not found`.
- [ ] **Step 2:** `go test ./bridge/` → FAIL.
- [ ] **Step 3:** Implement `bridge/catalog.go`.
- [ ] **Step 4:** `go test ./bridge/` → PASS.
- [ ] **Step 5:** Commit `feat(bridge): list_tables and describe_table`.

---

### Task 9: MCP server + CLI

**Files:**
- Create: `mcpserver/server.go`, `mcpserver/server_test.go`, `cmd/adminer-bridge/main.go`

**Interfaces:**
- Consumes: `bridge.Bridge` methods, `config.Load/DefaultPath`.
- Produces: `func mcpserver.New(b *bridge.Bridge, version string) *mcp.Server`.

**Tools** (names fixed):
| Tool | Input | Output |
|---|---|---|
| `list_environments` | `{}` | `{"environments": []EnvInfo}` |
| `query` | `{"env": string, "sql": string, "max_rows"?: int}` | `Result` |
| `list_tables` | `{"env": string, "schema"?: string}` | `Result` |
| `describe_table` | `{"env": string, "table": string}` | `TableDescription` |

Tool descriptions must tell the model: read-only; one SELECT or EXPLAIN (no ANALYZE) per call; only built-in Postgres functions; no `;` or newlines in literals (use `chr(59)`/`chr(10)`); results capped at 1000 rows; on a gate rejection, read the rule and rewrite. Errors are returned as tool errors (`IsError`), with the gate's message verbatim.

CLI: `adminer-bridge serve [--config path]` runs MCP over stdio; `adminer-bridge query [--config path] --env NAME [--max-rows N] [--json] "SQL"` prints a `text/tabwriter` table (NULL shown as `NULL`, plus `(N rows, truncated)` footer) or JSON. Exit code 1 on error with the error on stderr. Nothing but MCP traffic on stdout in `serve`.

- [ ] **Step 1: Write failing tests** in `mcpserver/server_test.go` using the go-sdk in-memory transport and a `bridge.NewWithExecutors` fake:
  - `TestListsFourTools`: tool names == `[describe_table list_environments list_tables query]`.
  - `TestQueryToolReturnsResult`: calling `query` returns structured content with `columns`, `rows`, `executed_sql`.
  - `TestQueryToolGateRejection`: `delete from users` → `IsError` true, text contains `rejected by read-only gate [statement_type]`.
- [ ] **Step 2:** `go get github.com/modelcontextprotocol/go-sdk@latest`; check its current API (context7); `go test ./mcpserver/` → FAIL.
- [ ] **Step 3:** Implement `mcpserver/server.go` and `cmd/adminer-bridge/main.go`.
- [ ] **Step 4:** `go test ./... ` → PASS; `go build -o adminer-bridge ./cmd/adminer-bridge` succeeds.
- [ ] **Step 5:** Commit `feat: MCP server and CLI`.

---

### Task 10: Integration tests, README, registration

**Files:**
- Create: `integration/integration_test.go`, `README.md`, `config.example.yaml`

Integration tests run only when `ADMINER_BRIDGE_IT=1` (skip otherwise), against the local env from Global Constraints, using a temp config file.

- [ ] **Step 1: Write the integration tests:**
  - `TestITSelect`: `select 1 as a, null as b` → `[["1", nil]]`.
  - `TestITFunctionsLoaded`: `select count(*) from pg_catalog.pg_class` succeeds; `select pg_sleep(0)` rejected.
  - `TestITLayer3`: build an `adminer.Client` directly and `Exec(gate.Wrap("CREATE TABLE zz_it_probe(i int)", ...), 0)` → block 4 `KindError` containing `read-only transaction`; then via bridge `select to_regclass('zz_it_probe')` returns NULL (table does not exist).
  - `TestITLockTimeout`: in a goroutine, hold `LOCK TABLE` on a scratch table via `docker exec glchat-be-database-1 psql` for 5s (skip if docker unavailable); bridge `select * from <table>` fails within ~3s with a lock-timeout error. Use a dedicated scratch table created and dropped by the test via psql, never an app table.
  - `TestITStatementTimeout`: env with `statement_timeout: 1s`, `select count(*) from generate_series(1, 300000000)` → error mentioning statement timeout.
  - `TestITRoleWarnings`: `ListEnvironments()` for local includes the superuser warning.
  - `TestITDescribeTable`: describe a table that exists in glchat (pick one from `ListTables`) → non-empty columns.
- [ ] **Step 2:** Run `ADMINER_BRIDGE_IT=1 ADMINER_LOCAL_PASSWORD=... go test ./integration/ -v` → PASS.
- [ ] **Step 3: Write `README.md`** (use the ste-writing style): what it is; build (`go build`, cgo note); config with `config.example.yaml` (local + a commented staging example); register with Claude Code:
  `claude mcp add adminer-bridge -e ADMINER_LOCAL_PASSWORD=... -- ~/Other/ok/adminer-bridge/adminer-bridge serve`;
  CLI usage; the three safety layers; **residual risks** (user-defined operators/types and custom type input functions are not checked; a dedicated read-only DB role is the real fix; the role check only warns); the curl `-F` pitfall.
- [ ] **Step 4:** `go vet ./... && go test ./...` → PASS.
- [ ] **Step 5:** Commit `test: integration tests; docs: README`.
