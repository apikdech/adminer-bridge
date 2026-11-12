# adminer-bridge

adminer-bridge runs read-only SQL against Postgres through an Adminer web page. It is an MCP server for Claude Code, and it also has a CLI. It is for developers who can reach a database only through Adminer.

## Prerequisites

- Go 1.27 or later.
- A C compiler. The SQL parser (`pg_query_go`) uses cgo.
- An Adminer instance with the `pgsql` driver, and a Postgres login for it.

## Build

```bash
go build
```

The command creates the `adminer-bridge` binary in the repository root.

## Configure

1. Copy `config.example.yaml` to `~/.config/adminer-bridge/config.yaml`.
2. Edit the `local` environment, and add more environments if needed (the file has a commented `staging` example).
3. Set the password in the environment variable that `password_env` names.

The default config path is `~/.config/adminer-bridge/config.yaml`. Use the `--config` flag or the `ADMINER_BRIDGE_CONFIG` variable to change it.

> **Caution:** Never put a password in the config file. The config file only names the environment variable.

| Field | Meaning | Default |
|---|---|---|
| `url` | Adminer base URL | required |
| `server`, `username`, `database` | Login values for Adminer | required |
| `password_env` | Name of the variable that holds the password | required |
| `schema` | Default schema | `public` |
| `statement_timeout` | Maximum run time of one statement | `15s` |
| `lock_timeout` | Maximum wait for a lock | `2s` |
| `http_timeout` | Timeout of one HTTP request to Adminer | `30s` |

The bridge runs one query at a time for each environment. Other calls wait.

## Register with Claude Code

```bash
claude mcp add adminer-bridge -e ADMINER_LOCAL_PASSWORD=... -- ~/Other/ok/adminer-bridge/adminer-bridge serve
```

The server provides four tools: `list_environments`, `query`, `list_tables`, and `describe_table`.

## Use the CLI

```bash
./adminer-bridge query --env local "select 1 as a"
./adminer-bridge query --env local --max-rows 10 --json "select * from users"
```

The default row limit is 100. The maximum is 1000. If the result is cut, the output says so. The CLI prints `NULL` for SQL null values. The JSON output uses `null`.

## Safety layers

All three layers always apply. The bridge sends no SQL to Adminer without them, including its own catalog queries.

1. **Parser.** The bridge parses the SQL with the real Postgres parser. It accepts one `SELECT` or `EXPLAIN` statement. It rejects `EXPLAIN ANALYZE`, `SELECT INTO`, locking clauses, and data-changing statements anywhere in the tree. It also rejects `$1`-style parameters.
2. **Functions.** The bridge accepts only functions in `pg_catalog`. It sends each function call as `pg_catalog.<name>`, so Postgres cannot pick a function with the same name from another schema. It also rejects a blocklist of dangerous names, for example `pg_sleep`, `set_config`, `pg_advisory_*`, `pg_read_file`, `lo_*`, `brin_*`, `pg_restore_*`, and functions that run SQL text such as `query_to_xml`, `ts_stat`, and `ts_rewrite`. The bridge reads the `pg_catalog` function list on the first connection. Until it has the list, it rejects all function calls.
3. **Read-only transaction.** The bridge sends the deparsed statement, not your text. The statement runs in `BEGIN TRANSACTION READ ONLY` with local timeouts, and ends with `ROLLBACK`. Postgres itself refuses writes.

## Residual risks

- A read-only transaction does not stop all side effects. Functions in `pg_catalog` are checked, but other paths exist.
- The gate does not check functions that the parser does not show as function calls:
  - attribute notation (`t.my_func` calls `my_func(t)`)
  - user-defined operators
  - user-defined casts and type input functions
- The read-only transaction is the backstop for these paths. It does not stop side effects of functions that do not write to the database.
- **The real fix is a dedicated read-only database role.** Use one when you can.
- On the first connection, the bridge checks the login role. It warns if the role is a superuser or if risky extensions or untrusted languages exist. The warnings appear in `list_environments`. The check only warns. It does not refuse to connect.

## Adminer login throttling

Adminer throttles an IP address after failed logins. The client protects against this:

- The client logs in directly. It does not send a probe request first.
- If a login fails, the client remembers the failure for 60 seconds. Calls in that time return the same error and send no credentials.
- The client never retries a failed login. It retries only once, and only for an expired session or an invalid CSRF token.

If a login fails, fix the password or the config. Then wait 60 seconds, or restart the server.

## Test

Run the unit tests:

```bash
go vet ./... && go test ./...
```

The integration tests need a running Adminer. They skip unless `ADMINER_BRIDGE_IT=1` is set. They use the local environment from the example config and one scratch table, `zz_it_lock_probe`, in the database container.

```bash
ADMINER_BRIDGE_IT=1 ADMINER_LOCAL_PASSWORD=... go test ./integration/ -v -count=1
```

## Known pitfall: curl `-F`

When you send forms to Adminer with curl, use `--form-string`. Do not use `-F`. The `-F` option treats `;` as an attribute separator and silently cuts the SQL batch.
