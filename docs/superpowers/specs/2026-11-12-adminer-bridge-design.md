# Adminer Bridge — Design

Date: 2026-11-12

## Why

The team's staging Postgres database is reachable **only** through an Adminer web UI. There is no VPN, SSH tunnel, port-forward or direct DB access. Debugging today means: type a query in Adminer, read the result, type the next query, open a new tab, repeat. An AI assistant cannot help because it cannot reach the database.

The bridge lets an LLM (Claude Code first, a team chat bot later) run **read-only** queries against a Postgres database through Adminer, quickly and safely. Target workflow: the user asks in plain language ("what chatbots can user X access?"), the LLM reads the application code to work out the tables and joins, calls the bridge, and answers, showing the SQL it ran.

## Goals

- An **MCP server** (stdio) that any MCP-capable LLM client can plug in with one config entry.
- Talks to Adminer over plain HTTP, the same requests a browser makes. No browser, no automation of a UI.
- **Read-only, guaranteed by deterministic code.** No LLM judgment in the safety path. A crafted query must not be able to write, lock, or do damage.
- Multiple named environments (`local`, `staging`, more later).
- Core logic in a reusable Go package so a Slack / Google Chat bot can be built on it later without a rewrite.

## Non-goals (for now)

- The Slack / Google Chat bot itself.
- Write access of any kind, including "write with confirmation".
- Databases other than Postgres, or Adminer drivers other than `pgsql`.
- A UI.
- Column redaction / data masking in v1. Decided by the user: staging holds mostly dummy data and sending query results to the LLM provider is acceptable. Revisit for the team bot or for any environment with real user data (a simple option then: per-environment column-name patterns whose values are replaced before results leave the bridge).

## Anti-goals (failure even if it "works")

- Any path, however creative, by which a query changes data, schema, settings, or other sessions.
- A query that stalls staging: holding locks or snapshots long enough to block migrations or pile up other sessions. (Every `SELECT` takes an AccessShareLock; the goal is that it is short-lived and gives up fast when it would wait, not that no lock is ever taken.)
- Credentials committed to a repo or printed in tool output / logs.
- A filter based on keyword or regex matching.

## Constraints

- Language: **Go** (user's choice). Go 1.27 is installed.
- Location: `/home/ricky-setiawan/Other/ok/adminer-bridge` (existing empty git repo). Personal tool, not part of the glchat repo.
- Adminer version on local test env: **5.4.1**. Staging is assumed to be similar; the builder should verify staging's version before relying on HTML details.
- Login is Adminer's own form only (driver, server, username, password, database). No SSO, basic auth or VPN in front.
- Test environment: local Adminer at `http://localhost:8080/?pgsql=database&username=postgres` (server `database`, user `postgres`). Build and test against this before adding staging.

## Architecture

```
LLM client ──stdio/MCP──> cmd/adminer-bridge (MCP server, thin)
                                   │
                          internal or pkg: bridge
                           ├── gate     (parse + validate + rewrite SQL)
                           ├── adminer  (HTTP client: login, session, run SQL, parse HTML)
                           └── config   (environments, credentials)
```

- `gate`: pure function `Validate(sql) -> (safeSQL, error)`. No I/O. Heavily unit-tested.
- `adminer`: HTTP client for one environment. Logs in, keeps the session cookie jar in memory, re-logs in automatically when the session expires (response is the login page), posts SQL to Adminer's SQL command page with the CSRF token, parses the HTML result table into columns and rows, detects Adminer error output.
- `bridge`: ties them together: `Query(env, sql, maxRows)` always runs the gate first. Nothing reaches `adminer` without passing the gate. This package is what a future bot would import.
- MCP layer: thin. Maps tools to `bridge` calls.
- Also a small CLI subcommand (e.g. `adminer-bridge query --env local "select 1"`) for manual testing without an LLM.

## The read-only gate (decided)

Three deterministic layers. All three always apply.

### Layer 1 — Real Postgres parser

Use `pg_query_go` (Postgres's own parser via libpg_query; requires cgo). Reject unless **all** hold:

- The input parses successfully.
- Exactly **one** statement.
- Top-level statement is `SelectStmt` or `ExplainStmt`. (Optionally `VariableShowStmt` / `SHOW`; builder's call.)
- `EXPLAIN` has no `ANALYZE` option (EXPLAIN ANALYZE executes the statement). The explained statement itself must also pass these rules.
- Walk the **entire** parse tree. Reject if any node anywhere is an `InsertStmt`, `UpdateStmt`, `DeleteStmt`, `MergeStmt` (data-modifying CTEs), or any other non-select statement node.
- No `SELECT ... INTO`.
- No locking clause (`FOR UPDATE / NO KEY UPDATE / SHARE / KEY SHARE`).

### Layer 2 — Function blocklist

A read-only transaction does not stop some functions. Walk every `FuncCall` in the tree; match on the unqualified, lower-cased function name (so `pg_catalog.PG_SLEEP` is caught). Block at least:

- Session / server control: `pg_terminate_backend`, `pg_cancel_backend`, `pg_reload_conf`, `pg_rotate_logfile`, `pg_switch_wal`, `pg_create_restore_point`, `pg_promote`, `pg_log_backend_memory_contexts`, `set_config`
- Sleeping / DoS: `pg_sleep`, `pg_sleep_for`, `pg_sleep_until`
- Locks: all `pg_advisory_*` and `pg_try_advisory_*`
- Filesystem: `pg_read_file`, `pg_read_binary_file`, `pg_ls_dir` and all `pg_ls_*`, `pg_stat_file`, all `lo_*` (large objects)
- Remote connections: all `dblink*`
- Notifications: `pg_notify`
- **Functions that execute SQL strings** (would bypass layer 1 and 2): `query_to_xml`, `query_to_xml_and_xmlschema`, `query_to_xmlschema`, `cursor_to_xml`, `cursor_to_xmlschema`, and the `*_to_xml*` family (`table_to_xml`, `schema_to_xml`, `database_to_xml`, etc.).
- Replication slots: all `pg_*replication_slot*`, `pg_logical_*`

The builder should research for further entries (e.g. by scanning `pg_proc` for volatile functions in `pg_catalog`).

**Built-ins only (decided — Claude's call after review).** A read-only transaction stops writes but not side effects from functions that open remote connections, touch files, or call out from untrusted languages (`dblink`, `postgres_fdw`, `plpython3u`, …). So in addition to the blocklist, the gate allows **only functions that exist in `pg_catalog`**. On first connect to an environment, the bridge fetches the set of `pg_catalog` function names (one fixed catalog query, itself gated) and caches it per environment. Any function call whose name is not in that set — user-defined functions, extension functions installed in `public` such as `dblink*`, anything schema-qualified to a non-`pg_catalog` schema — is rejected. Blocklisted names are rejected even though they are in `pg_catalog`. Aggregates and window functions count as functions here too (they are in `pg_catalog`). Until the set is loaded, every function call is rejected.

**Role check (decided — Claude's call).** On first connect, the bridge checks whether the login role is a superuser and which extensions and untrusted languages are installed, and reports this via `list_environments` (and a warning in logs). It warns, it does not refuse: the staging login may be a privileged role the user cannot change. A dedicated read-only DB role remains the recommended setup and the docs/README should say so.

### Layer 3 — Postgres enforces read-only

The bridge never sends the user's raw text. It sends the **deparsed** output of the parse tree (`pg_query` deparse), so Adminer's own statement splitter and Postgres see exactly what the gate approved. If deparse fails, reject. The deparsed statement is wrapped:

```sql
BEGIN TRANSACTION READ ONLY;
SET LOCAL statement_timeout = '<timeout>';
SET LOCAL lock_timeout = '<lock_timeout>';
SET LOCAL idle_in_transaction_session_timeout = '<timeout>';
<deparsed statement>;
ROLLBACK;
```

Adminer must be told to stop on the first error (its "stop on error" option), so a failed `BEGIN` cannot lead to the statement running outside the transaction. If the bridge cannot confirm the `BEGIN` succeeded from the response, it reports an error.

### Gate tests (required before any network code is trusted)

A table-driven test corpus of bypass attempts that must all be rejected, including at least: multiple statements (`select 1; drop table x`), comment tricks (`select 1 /* ; */ ; delete ...`, `--` line comments), `COMMIT;` / `ROLLBACK;` / `END;` escapes, data-modifying CTEs, `SELECT INTO`, `FOR UPDATE`, `EXPLAIN ANALYZE delete ...`, every blocked function (plain, schema-qualified, mixed-case, quoted identifiers), `query_to_xml('delete ...', ...)`, `COPY`, `SET`, `DO $$ ... $$`, `CALL`, `PREPARE` / `EXECUTE`, `LISTEN` / `NOTIFY`, `VACUUM`, dollar-quoted strings hiding semicolons, unicode escapes in identifiers (`U&"..."`). Plus a set of normal queries that must pass (joins, CTEs, window functions, subqueries, `EXPLAIN`).

## MCP tools

Exact names and schemas are the builder's call; the minimum set:

| Tool | Input | Output |
|---|---|---|
| `list_environments` | — | env names (never credentials) |
| `query` | `env`, `sql`, optional `max_rows` | `columns`, `rows`, `row_count`, `truncated`, `executed_sql` (the deparsed statement) |
| `list_tables` | `env`, optional `schema` | tables with schema |
| `describe_table` | `env`, `table` (optionally schema-qualified) | columns, types, nullability, defaults, indexes, foreign keys |

`list_tables` and `describe_table` are implemented as fixed catalog queries that still go through the gate (no special bypass path).

Gate rejections return a clear error naming the rule that was broken, so the LLM can rewrite the query.

Output values: SQL `NULL` must be distinguishable from the string `"NULL"`. Large results are capped by `max_rows` with a sane default and hard maximum (builder's call, e.g. 100 default / 1000 max) and `truncated: true`.

## Configuration

- Config file outside any repo, e.g. `~/.config/adminer-bridge/config.yaml`, path overridable by env var/flag.
- Per environment: name, Adminer base URL, server, username, database, optional default schema, and a **reference** to the password (name of an env var), not the password itself. Builder may additionally support the password in the file with a warning, but env var is the default.
- Per environment optional `statement_timeout` (default **15s**), `lock_timeout` (default **2s**, so a query gives up rather than queue behind a migration), and HTTP timeout.
- At most **one** query in flight per environment at a time (calls queue in the bridge). Decided — Claude's call, to keep load on staging low.
- Passwords and session cookies never appear in tool output or logs.

## Adminer protocol notes (to verify against 5.4.1)

- Login: POST the login form with `auth[driver]=pgsql`, `auth[server]`, `auth[username]`, `auth[password]`, `auth[db]`. Session lives in cookies.
- SQL command: the page `?pgsql=<server>&username=<user>&db=<db>&sql=`; POST with the query text, the CSRF `token` from the page, the row limit and the stop-on-error option.
- Results are HTML tables; errors are rendered as error paragraphs. Each statement in the batch produces its own output block; the bridge must pick out the block for the user's statement and confirm `BEGIN` and `SET LOCAL` succeeded.
- Builder must confirm how Adminer renders NULL, long values (whether it truncates), binary data, and row limits on the SQL command page, using the local instance.

## Testing

- Gate: unit tests as above. This is the most important test suite.
- Adminer client: tests against recorded HTML fixtures from local Adminer 5.4.1 (login page, result page, error page, expired session).
- Integration: an opt-in test (build tag or env var) that runs against `localhost:8080`, including proving a write attempt fails at layer 3 when layers 1–2 are artificially bypassed in the test.

## Future: team bot (not built now, design must not block it)

- The bot imports the `bridge` package.
- Will need: an audit log (who asked, SQL run, row count), per-user or per-channel access control, and care about posting staging user data into team channels (e.g. masking, or replying privately). Noted so the package API leaves room (e.g. a caller identity / audit hook), not built now.

## Left to the builder

Package layout, MCP SDK choice (the official `github.com/modelcontextprotocol/go-sdk` is the default suggestion), HTML parsing library, exact tool names and JSON shapes, row-limit defaults, CLI shape, config format details, logging.
