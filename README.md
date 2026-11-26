# adminer-bridge

adminer-bridge runs read-only SQL against Postgres through an Adminer web page. It is an MCP server for clients such as Claude Code, Codex, and OpenCode. It also has a CLI. It is for developers who can reach a database only through Adminer.

## Prerequisites

- Go 1.27 or later.
- A C compiler. The SQL parser (`pg_query_go`) uses cgo.
- An Adminer instance with the `pgsql` driver, and a Postgres login for it.

## Build

```bash
go build -o adminer-bridge ./cmd/adminer-bridge
```

The command creates the `adminer-bridge` binary in the repository root.

## Configure

This README uses the term **password variable** for the environment variable that holds the password of one environment. The `password_env` field in the config gives its name.

1. Copy `config.example.yaml` to `~/.config/adminer-bridge/config.yaml`.
2. Edit the `local` environment. The file also has a commented `staging` example.
3. Set the password variable of each environment in your shell.

The default config path is `~/.config/adminer-bridge/config.yaml`. To use a different path, use the `--config` flag or the `ADMINER_BRIDGE_CONFIG` variable.

> **Caution:** Never put a password in the config file. The config file only gives the name of the password variable.

| Field | Meaning | Default |
|---|---|---|
| `url` | Adminer base URL | required |
| `server`, `username` | Login values for Adminer | required |
| `database` | Default database. The login uses it. | required |
| `databases` | Allowed databases. See [Use more than one database](#use-more-than-one-database). | `[database]` |
| `password_env` | Name of the password variable | required |
| `schema` | Default schema | `public` |
| `statement_timeout` | Maximum run time of one statement | `15s` |
| `lock_timeout` | Maximum wait for a lock | `2s` |
| `http_timeout` | Timeout of one HTTP request to Adminer | `30s` |

`statement_timeout` and `lock_timeout` must be at least `1ms`. `http_timeout` must be greater than `statement_timeout`.

The bridge runs one query at a time for each environment, for all its databases together. Other calls wait.

## Use more than one database

One Adminer login can reach more than one database on the same server. For example: `staging`, `dev`, and `dev2`. Use the `databases` field to select the databases that a call can use. You do not need one environment for each database.

> **Caution:** Each database in the list must exist, and the login role must be able to connect to it. Adminer counts a database that it cannot open as a failed login, and it ends the session for all databases of the environment. Too many failed logins block your IP address.
>
> The bridge protects against this. Before the first call to a database that is not the default database, it checks the database in the default database. If the database does not exist or the role cannot connect, the call fails with an error. The bridge sends nothing to Adminer with that database name. It remembers this result for 60 seconds. If Adminer still cannot open the database, the bridge does not log in again. Calls to that database then fail for 60 seconds and send no requests. After that, the bridge checks the database again in the default database. An expired session is different: if Adminer says "Session expired", the bridge logs in again one time, as for the default database.

1. Add the `databases` list to the environment:

   ```yaml
   environments:
     staging:
       # url, server, username, password_env as before
       database: staging                  # default database
       databases: [staging, dev, dev2]    # allowed databases
   ```

2. Make sure that the list includes the value of `database`. If it does not, the config does not load.
3. Make sure that each entry is a database name, and that no name occurs two times. An entry must not start or end with a space.

These rules apply:

- If you do not set `databases`, the only allowed database is `database`.
- An empty list, `databases: []`, is an error. The config does not load.
- The `schema` of the environment applies to all its databases.
- One login covers all allowed databases of the environment. The bridge changes only the database in the Adminer URL.
- A call without a database uses the default database.
- The name must match exactly. The match is case-sensitive.
- The bridge rejects a database that is not in the list. It rejects it before it sends anything to Adminer. The error gives the allowed databases.
- On the first call to each database, the bridge reads the function list and checks the login role in that database.

## Use pgAdmin instead of Adminer

Some databases can be reached only through pgAdmin 4. Set `backend: pgadmin` on
the environment. The bridge then logs in to pgAdmin and runs the queries in
pgAdmin's query tool. The safety layers are the same.

| Field | Meaning for `backend: pgadmin` |
|---|---|
| `url` | pgAdmin base URL |
| `server` | Server name in the pgAdmin tree, for example `koi-dev`. pgAdmin must have its password saved. |
| `username` | pgAdmin login email |
| `password_env` | Name of the variable that holds the pgAdmin password |
| `database`, `databases` | As for Adminer. Each database must be on that server. |

```yaml
environments:
  claudia-dev:
    backend: pgadmin
    url: https://pgadmin.example.com/
    server: koi-dev
    username: bot@example.com
    database: glchat_dev
    password_env: PGADMIN_PASSWORD
```

How it works, and what is different from Adminer:

- pgAdmin has no public API for SQL. The bridge uses the endpoints of the pgAdmin
  web page. They were captured from pgAdmin 9.8 (`pgadmin/testdata/NOTES.md`). A
  pgAdmin upgrade can change them.
- pgAdmin returns only the last result of a batch. The bridge runs the read-only
  wrapper one statement at a time on one query tool connection. Before your
  statement it checks `transaction_read_only` and stops if it is not `on`. After
  `BEGIN`, it always sends `ROLLBACK`, also after an error.
- The bridge opens one query tool for each database and uses it again.
- pgAdmin locks an account after a few failed logins. After a failed login the
  bridge does not try again for 60 seconds. Check the email and password first.

> **Caution:** Use a pgAdmin login and a database role for the bot, not a personal
> account, when you can.

## Try the local environment first

Test with the `local` environment before you add staging.

1. Build the binary. See [Build](#build).
2. Create the config directory:

   ```bash
   mkdir -p ~/.config/adminer-bridge
   ```

3. Copy the example config. It contains only the `local` environment:

   ```bash
   cp config.example.yaml ~/.config/adminer-bridge/config.yaml
   ```

4. Set the local password variable:

   ```bash
   export ADMINER_LOCAL_PASSWORD=$(docker inspect glchat-be-database-1 --format '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^POSTGRES_PASSWORD=//p')
   ```

5. Run a query with the CLI:

   ```bash
   ./adminer-bridge query --env local "select current_user, version()"
   ```

   The CLI prints one row. This shows that the login and the safety layers work. On the local database, stderr also shows a superuser warning. This is normal.

6. Register the MCP server with your client. See [Register with an MCP client](#register-with-an-mcp-client).
7. In the client, ask the model to call `list_environments`.

   The result shows each environment with its default database and its allowed databases.

## Add staging later

> **Caution:** The server reads the password variable of every environment in the config when it starts. If one password variable is not set, the server does not start, and no environment works. Add an environment to the config only when you can set its password variable.

1. Get a staging login. If you can, ask for a dedicated read-only role.
2. Find the Adminer version on staging. The version shows at the bottom of the Adminer login page.

   The bridge reads the HTML of Adminer 5.4.1. If staging uses a different version, test with care.

3. Copy the commented `staging` example from `config.example.yaml` into `~/.config/adminer-bridge/config.yaml`.
4. Change the values of the `staging` entry to your staging login. Keep `password_env: ADMINER_STAGING_PASSWORD`.
5. Set the staging password variable:

   ```bash
   export ADMINER_STAGING_PASSWORD='...'
   ```

   > **Caution:** If the next step fails with a login error, do not try again many times. Adminer blocks your IP address after too many failed logins. Fix the config or the password first.

6. Run a query on staging with the CLI:

   ```bash
   ./adminer-bridge query --env staging "select 1"
   ```

   The CLI prints one row.

7. Add `ADMINER_STAGING_PASSWORD` to your MCP client config. Each client section below shows where.
8. Restart the MCP client.

## Register with an MCP client

The server gives four tools: `list_environments`, `query`, `list_tables`, and `describe_table`.

- `list_environments` gives each environment with its URL, default schema, default database, and allowed databases. For each allowed database, it shows if the bridge is connected, and the role facts and warnings.
- `query`, `list_tables`, and `describe_table` have an optional `database` parameter. If you do not give it, the tool uses the default database. The value must be an allowed database of the environment.
- Each result contains a `database` field. It shows the database that gave the result.

The server reads passwords from the password variables. Each client passes these variables to the server. In each client config, use the absolute path of the binary.

### Claude Code

Run this command:

```bash
claude mcp add adminer-bridge -e ADMINER_LOCAL_PASSWORD=... -- ~/Other/ok/adminer-bridge/adminer-bridge serve
```

For staging, also add `-e ADMINER_STAGING_PASSWORD=...` to the command.

### Codex

1. Add this entry to `~/.codex/config.toml`:

   ```toml
   [mcp_servers.adminer-bridge]
   command = "~/Other/ok/adminer-bridge/adminer-bridge"
   args = ["serve"]
   # Codex copies these variables from the shell that starts it.
   # For staging, add "ADMINER_STAGING_PASSWORD" to this list.
   env_vars = ["ADMINER_LOCAL_PASSWORD"]
   ```

2. Set the password variables in your shell.
3. Start `codex` from the same shell.
4. To see the configured servers, run `codex mcp list`.

With `env_vars`, the passwords do not go into `config.toml`.

> **Caution:** The `codex mcp add --env` command writes the password into `config.toml` as plain text. Use it only if you accept this risk.

```bash
codex mcp add adminer-bridge --env ADMINER_LOCAL_PASSWORD=... -- ~/Other/ok/adminer-bridge/adminer-bridge serve
```

### OpenCode

1. Add this entry to `~/.config/opencode/opencode.json`. For one project only, use `opencode.json` in the project root.

   ```jsonc
   {
     "$schema": "https://opencode.ai/config.json",
     "mcp": {
       "adminer-bridge": {
         "type": "local",
         "command": ["~/Other/ok/adminer-bridge/adminer-bridge", "serve"],
         "enabled": true,
         // {env:NAME} copies the variable from the shell that starts OpenCode.
         // For staging, add "ADMINER_STAGING_PASSWORD": "{env:ADMINER_STAGING_PASSWORD}".
         "environment": {
           "ADMINER_LOCAL_PASSWORD": "{env:ADMINER_LOCAL_PASSWORD}",
           "ADMINER_STAGING_PASSWORD": "{env:ADMINER_STAGING_PASSWORD}",
         },
         // Keep this longer than the 30s HTTP timeout of the bridge.
         // The bridge then reports a slow query as its own error.
         "timeout": 45000
       }
     }
   }
   ```

2. Set the password variables in your shell.
3. Start `opencode` from the same shell.

With `{env:NAME}`, the passwords do not go into `opencode.json`. If a variable is not set, OpenCode gives an empty value, and the server does not start.

## Run as an HTTP server (Docker)

Use HTTP mode when the MCP client runs in a container that must not hold secrets, for example a debug agent. The bridge runs in its own container and holds the passwords. The agent calls the bridge over the network and holds only the bearer token.

In HTTP mode, the bridge serves MCP over streamable HTTP at `/mcp`. All clients share one Adminer login and one query lock for each environment. `GET /healthz` returns `ok` and needs no token. All other paths return 404.

```text
adminer-bridge serve --http ADDR [--config path] [--token-env NAME]
adminer-bridge health [--url URL]
```

### Create the token

The bearer token is the only access control of HTTP mode.

> **Warning:** A person with the token can run read-only queries on every configured environment and database. Keep the token secret. Do not put it in an image, a config file, or a log.

1. Create a token:

   ```bash
   openssl rand -hex 32
   ```

2. Give the token to the bridge in the `ADMINER_BRIDGE_TOKEN` variable. To use a different variable, use `--token-env NAME`.
3. Give the same token to the MCP client as the header `Authorization: Bearer <token>`.

If the token variable is not set, or if the token has fewer than 32 characters, `serve --http` stops with an error. The error gives the name of the variable, not the value. A request with a missing or wrong token gets a 401 response, and the bridge writes one log line without the token.

### Build and run the image

The image uses `gcr.io/distroless/static-debian12:nonroot`, and the binary is linked statically. The image is about 21 MB. It has no libc, no shell, and no curl. It runs as user 65532. The Dockerfile pins the base images by tag. For reproducible builds, you can pin them by digest. By default it runs `serve --http :8090` and reads the config from `/etc/adminer-bridge/config.yaml`. The Docker health check runs `adminer-bridge health`.

1. Build the image:

   ```bash
   docker build -t adminer-bridge:latest .
   ```

2. Put the token and the password variables in an env file, for example `adminer-bridge.env`. Keep this file out of git.

   ```bash
   ADMINER_BRIDGE_TOKEN=<token>
   ADMINER_STAGING_PASSWORD=<password>
   ```

3. Start the container:

   ```bash
   docker run -d --name adminer-bridge --read-only --cap-drop ALL \
     --env-file adminer-bridge.env \
     -v "$PWD/config.yaml:/etc/adminer-bridge/config.yaml:ro" \
     adminer-bridge:latest
   ```

4. Make sure that the log shows the listen address:

   ```bash
   docker logs adminer-bridge
   ```

   The log shows `listening on [::]:8090`.

> **Note:** If you change the listen port, also change the health check: `health --url http://127.0.0.1:PORT/healthz`.

The server has no write timeout. A tool call can take as long as the longest `http_timeout` of an environment, and the event stream of a session stays open. A write timeout would stop both. Request bodies are limited to 1 MiB. A session closes after one hour without requests. On `SIGINT` or `SIGTERM`, the server stops gracefully.

### Connect an MCP client over HTTP

The MCP URL is `http://HOST:8090/mcp`. A generic client config has this shape:

```json
{
  "mcpServers": {
    "adminer-bridge": {
      "type": "http",
      "url": "http://10.88.0.20:8090/mcp",
      "headers": { "Authorization": "Bearer ${ADMINER_BRIDGE_TOKEN}" }
    }
  }
}
```

The field names and the variable syntax are different in each client. Check your client's docs.

For Claude Code, run this command:

```bash
claude mcp add --transport http adminer-bridge http://HOST:8090/mcp --header "Authorization: Bearer $TOKEN"
```

### Run next to the the app VPN sidecar

The staging Adminer is reachable only through the VPN. In `your docker-compose.yml`, the `vpn` service runs OpenVPN. The `app` service uses `network_mode: "service:vpn"` and listens on port 8080 in that network namespace. Run the bridge in the same namespace.

The agent containers use a separate internal network, `agents_net`. The bridge listens only on the address of the `vpn` service in `agents_net`.

1. Add the `agents_net` network to the top-level `networks` map:

   ```yaml
   networks:
     app_net:
       ipam:
         config:
           - subnet: 10.87.0.0/24
     agents_net:
       internal: true
       ipam:
         config:
           - subnet: 10.88.0.0/24
   ```

2. Connect the `vpn` service to both networks. This map form replaces the existing list form `networks: - app_net`.

   ```yaml
   vpn:
     networks:
       app_net: {}
       agents_net:
         ipv4_address: 10.88.0.20
   ```

3. Add the bridge service:

   ```yaml
   adminer-bridge:
     image: adminer-bridge:latest   # or build: from this repo
     restart: unless-stopped
     network_mode: "service:vpn"    # staging Adminer is reachable only through the VPN
     depends_on: { vpn: { condition: service_healthy } }
     command: ["serve", "--http", "10.88.0.20:8090"]
     healthcheck:
       test: ["CMD", "/adminer-bridge", "health", "--url", "http://10.88.0.20:8090/healthz"]
     env_file: ../adminer-bridge/secrets.env   # ADMINER_BRIDGE_TOKEN, ADMINER_STAGING_PASSWORD
     volumes:
       - ../adminer-bridge.yaml:/etc/adminer-bridge/config.yaml:ro
     read_only: true
     cap_drop: [ALL]
   ```

4. Connect each agent container to `agents_net` only:

   ```yaml
   debug-agent:
     networks: [agents_net]
   ```

5. Configure the agent container to use `http://10.88.0.20:8090/mcp`.
6. If `app` calls the bridge, configure it to use `http://10.88.0.20:8090/mcp`. `app` is in the same namespace, so it can reach this address.

The reasons for this setup:

- The bridge listens only on `10.88.0.20`. It does not listen on the OpenVPN `tun` interface, so hosts on the corporate VPN cannot reach it.
- `agents_net` is internal. Containers on it have no internet access.
- An agent container on `agents_net` can reach only the services on `agents_net`. Later services for agents, for example `llm-proxy` and `opensearch-mcp`, can also join `agents_net`.
- The env file is not in `../secrets`. The upstream compose file mounts all of `../secrets` into `app`, so `app` could read a file there.
- The bridge uses port 8090 because `app` already uses port 8080 in the same namespace.
- The services address each other by static IP. VPN DNS can replace `resolv.conf`, so Docker DNS names can stop working.
- Do not publish port 8090 on the host.
- The Adminer URL in the bridge config might need an IP address. If the host name resolves only through VPN DNS, set `VPN_USE_PUSHED_DNS` on the `vpn` service. This is the same caveat as for the app.

### Security notes

- The bearer token is the only access control. A person with the token can run read-only queries on every configured environment and database.
- Keep port 8090 off public networks. The bridge has no TLS.
- The bridge compares tokens in constant time. It does not log tokens, passwords, cookies, or query results.

## Use the CLI

```bash
./adminer-bridge query --env local "select 1 as a"
./adminer-bridge query --env local --max-rows 10 --json "select * from users"
./adminer-bridge query --env local --db postgres "select current_database()"
```

To use a database other than the default database, use `--db NAME`. The database must be an allowed database of the environment.

The default row limit is 100. The maximum is 1000. If the result is cut, the output says so. The CLI prints `NULL` for SQL null values. The JSON output uses `null`.

## Safety layers

All three layers always apply. The bridge sends no SQL to Adminer without them, including its own catalog queries.

1. **Parser.** The bridge parses the SQL with the real Postgres parser. It accepts one `SELECT` or `EXPLAIN` statement. It rejects `EXPLAIN ANALYZE`, `SELECT INTO`, locking clauses, and data-changing statements anywhere in the tree. It also rejects `$1`-style parameters.
2. **Functions.** The bridge accepts only functions in `pg_catalog`. It sends each function call as `pg_catalog.<name>`, so Postgres cannot pick a function with the same name from another schema. It also rejects a blocklist of dangerous names, for example `pg_sleep`, `set_config`, `pg_advisory_*`, `pg_read_file`, `lo_*`, `brin_*`, `pg_restore_*`, and functions that run SQL text such as `query_to_xml`, `ts_stat`, and `ts_rewrite`. The bridge reads the `pg_catalog` function list on the first connection to each database. Until it has the list, it rejects all function calls.
3. **Read-only transaction.** The bridge sends the deparsed statement, not your text. The statement runs in `BEGIN TRANSACTION READ ONLY` with local timeouts, and ends with `ROLLBACK`. Postgres itself refuses writes.

## Residual risks

- A read-only transaction does not stop all side effects. The gate checks functions in `pg_catalog`, but other paths exist.
- The gate does not check functions that the parser does not show as function calls:
  - attribute notation (`t.my_func` calls `my_func(t)`)
  - user-defined operators
  - user-defined casts and type input functions
- The read-only transaction is the backstop for these paths. It does not stop side effects of functions that do not write to the database.
- Adminer runs a hidden `EXPLAIN` for each statement that starts with `SELECT`. It runs it on a second database connection, after the statement and before the `ROLLBACK`. That connection is outside the read-only transaction and has no statement or lock timeouts. `EXPLAIN` without `ANALYZE` only plans the statement and does not run it. But planning takes table locks: if a migration is waiting for a lock, the plan can wait behind it while the bridge's transaction stays open. In the worst case, the migration waits until `idle_in_transaction_session_timeout` (the same as `statement_timeout`, 15s by default) ends the bridge's transaction. The bridge cannot turn this `EXPLAIN` off, because the Adminer option that does so also removes the statement echoes that the bridge checks.
- **The real fix is a dedicated read-only database role.** Use one when you can.
- On the first connection to each database, the bridge checks the login role. It warns if the role is a superuser or if risky extensions or untrusted languages exist. The warnings appear in `list_environments`, for each database. The check only warns. It does not refuse to connect.

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

The integration tests need a running Adminer. They skip unless `ADMINER_BRIDGE_IT=1` is set. They use the local environment with the allowed databases `glchat` and `postgres`, and one scratch table, `zz_it_lock_probe`, in the database container. A test also checks that the bridge rejects `stackframe`, a database that is not in the list.

```bash
ADMINER_BRIDGE_IT=1 ADMINER_LOCAL_PASSWORD=... go test ./integration/ -v -count=1
```

## Known pitfall: curl `-F`

When you send forms to Adminer with curl, use `--form-string`. Do not use `-F`. The `-F` option treats `;` as an attribute separator and silently cuts the SQL batch.
