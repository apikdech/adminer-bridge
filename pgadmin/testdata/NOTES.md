# pgAdmin 9.8 query tool protocol (captured 2026-11-26)

Captured against a real pgAdmin 9.8 (asset tag `?ver=90800`) with a Python
probe. `client_test.go` rebuilds these shapes in a fake server. There is no
public API for running SQL; these are the endpoints of pgAdmin's own web UI.

## Login

- `GET /login` with `Accept: application/json` answers
  `{"meta":{"code":200},"response":{"csrf_token":"…","identity_attributes":["email"]}}`.
  The HTML login page (any other Accept) has no token.
- `POST /authenticate/login`, form: `csrf_token`, `email`, `password`,
  `language=en`, `internal_button=Login`. Success redirects to `/browser/`;
  failure redirects to `/login`. Cookie: `pga4_session`.
- The same token goes in `X-pgA-CSRFToken` on every API call.
- Over HTTPS every API call needs a `Referer` on the pgAdmin origin. Without
  it: HTTP 401, `"The referrer header is missing. You need to refresh the page."`.
- The instance allows only internal (email and password) logins. pgAdmin
  locks an account after a few failed logins (`MAX_LOGIN_ATTEMPTS`).

## Tree

- `GET /browser/server_group/nodes/` → `data: [{"_id":"6","label":"Servers",…}]`.
- `GET /browser/server/nodes/<sgid>` → nodes in **`result`** (not `data`):
  `{"_id":"14","label":"koi-dev","connected":false,"is_password_saved":true,"username":"glchat_team",…}`.
- `POST /browser/server/connect/<sgid>/<sid>` with `{}` connects with the saved
  password: `{"success":1,"info":"Server connected.",…}`.
- `GET /browser/database/nodes/<sgid>/<sid>/` → `data: [{"_id":2165387,"label":"glchat_dev",…}]`
  (`_id` is a number here, a string for groups and servers).

## Query tool

- `POST /sqleditor/initialize/sqleditor/<trans_id>/<sgid>/<sid>/<did>` with
  `{"dbname":…,"user":…,"role":null,"password":null}`; without `dbname`:
  HTTP 500 `"errormsg":"'dbname'"`. `trans_id` is chosen by the client.
- `POST /sqleditor/query_tool/start/<trans_id>` with `{"sql":…,"explain_plan":null}`
  → `data.status: -1` (started).
- `GET /sqleditor/poll/<trans_id>` → `data.status` `"Busy"` or `"Success"`:
  - command (BEGIN, SET, ROLLBACK): `result: "BEGIN"`, `colinfo: null`.
  - rows: `result: [[1, null, true, "1.5", "{\"a\": 1}"]]` (int as number,
    numeric and bigint as strings, json as text), `colinfo: [{"name":…,"pos":…},…]`,
    `rows_affected`: total rows. At most 1000 rows per poll.
  - no rows: `colinfo` set, `result: "SELECT 0"`.
  - error: HTTP 500, `success: 0`, `errormsg: "ERROR:  relation … does not exist\nLINE 1: …\n\nSQL state: 42P01…"`.
  - `transaction_status`: 0 idle, 2 in transaction, 3 aborted.
- `GET /sqleditor/fetch_window/<trans_id>/<from>/<to>` (1-based, inclusive) →
  `data.result` rows. Past the end: HTTP 500 `"position out of bound"`.
- `DELETE /sqleditor/close/<trans_id>`.
- Unknown `trans_id`: HTTP 404, `info: "DATAGRID_TRANSACTION_REQUIRED"`.

## Transactions

Only the last result of a multi-statement query comes back, so the client
runs one statement per call. An explicit `BEGIN TRANSACTION READ ONLY` stays
open across calls (status 2); `current_setting('transaction_read_only')` is
`on` inside it and `off` after `ROLLBACK`. A write inside it fails with
`cannot execute CREATE TABLE in a read-only transaction` (SQL state 25006)
and leaves status 3 until `ROLLBACK`.
