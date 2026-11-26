// Package pgadmin runs the bridge's wrapped batches through pgAdmin 4's query
// tool, for databases that can be reached only through pgAdmin.
//
// The endpoints are pgAdmin's internal web API (there is no public one); they
// were captured from pgAdmin 9.8, see testdata/NOTES.md. pgAdmin returns only
// the last result of a multi-statement query, so the client runs the
// statements of a batch one at a time on one query tool connection. An
// explicit BEGIN keeps the transaction open across the calls.
//
// Safety, in addition to the bridge's gate:
//   - Exec accepts only batches that start with BEGIN TRANSACTION READ ONLY
//     and end with ROLLBACK.
//   - Before the caller's statement it checks transaction_read_only in the
//     open transaction and refuses to continue unless it is "on".
//   - Once BEGIN succeeded, it always sends ROLLBACK, also after an error.
//   - A failed login is never retried within loginCooldown: pgAdmin locks an
//     account after a few failed logins.
package pgadmin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"adminer-bridge/adminer"
	"adminer-bridge/config"
)

// ErrLogin means pgAdmin rejected the login.
var ErrLogin = errors.New("pgadmin login failed")

// errSession means pgAdmin no longer knows the session or the query tool;
// Exec logs in or opens the tool again once.
var errSession = errors.New("pgadmin session or query tool expired")

const (
	loginCooldown = 60 * time.Second
	fetchPage     = 1000
	maxRows       = 100000 // cap for limit 0 (the bridge's catalog queries)
	maxBodyBytes  = 64 << 20

	beginStmt     = "BEGIN TRANSACTION READ ONLY"
	rollbackStmt  = "ROLLBACK"
	readOnlyCheck = "SELECT pg_catalog.current_setting('transaction_read_only')"
)

// Client holds one pgAdmin session for an environment.
type Client struct {
	env       *config.Env
	password  string
	base      string
	http      *http.Client
	now       func() time.Time
	pollEvery time.Duration

	mu       sync.Mutex
	csrf     string // set while logged in
	loginErr error
	loginAt  time.Time
	sgid     string
	sid      string
	dbUser   string
	tools    map[string]string // database -> query tool (transaction) id
}

// NewClient prepares a client for env. It reads the password but makes no
// requests.
func NewClient(env *config.Env) (*Client, error) {
	pw, err := env.Password()
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(env.URL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("environment %q: invalid pgAdmin url", env.Name)
	}
	jar, _ := cookiejar.New(nil)
	return &Client{
		env: env, password: pw, base: strings.TrimRight(u.String(), "/"),
		http:      &http.Client{Jar: jar, Timeout: env.HTTPTimeout},
		now:       time.Now,
		pollEvery: 200 * time.Millisecond,
		tools:     map[string]string{},
	}, nil
}

// Exec runs a batch made by gate.Wrap in database and returns one block per
// statement, like Adminer with error_stops: a failing statement is the last
// block. limit caps the rows of a result (0 = up to maxRows).
func (c *Client) Exec(ctx context.Context, database, batch string, limit int) ([]adminer.Block, error) {
	if database == "" {
		return nil, fmt.Errorf("environment %q: no database given", c.env.Name)
	}
	stmts, err := splitBatch(batch)
	if err != nil {
		return nil, fmt.Errorf("environment %q: %w", c.env.Name, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	for attempt := 0; ; attempt++ {
		blocks, err := c.execOnce(ctx, database, stmts, limit)
		if errors.Is(err, errSession) && attempt == 0 {
			c.reset()
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("environment %q, database %q: %w", c.env.Name, database, err)
		}
		return blocks, nil
	}
}

// splitBatch splits a gate.Wrap batch into its statements. The gate never
// lets ";" or a line break into the caller's statement, so ";\n" separates
// statements exactly.
func splitBatch(batch string) ([]string, error) {
	if !strings.HasSuffix(batch, ";") {
		return nil, errors.New("pgadmin: batch is not a wrapped read-only batch")
	}
	stmts := strings.Split(strings.TrimSuffix(batch, ";"), ";\n")
	if len(stmts) < 3 || stmts[0] != beginStmt || stmts[len(stmts)-1] != rollbackStmt {
		return nil, errors.New("pgadmin: batch is not a wrapped read-only batch")
	}
	return stmts, nil
}

// reset forgets the session (with its cookies, so the next /login is not
// redirected as logged in) and every query tool.
func (c *Client) reset() {
	jar, _ := cookiejar.New(nil)
	c.http.Jar = jar
	c.csrf = ""
	c.sgid, c.sid, c.dbUser = "", "", ""
	c.tools = map[string]string{}
}

func (c *Client) execOnce(ctx context.Context, database string, stmts []string, limit int) (blocks []adminer.Block, err error) {
	if err := c.ensureLogin(ctx); err != nil {
		return nil, err
	}
	trans, err := c.ensureTool(ctx, database)
	if err != nil {
		return nil, err
	}

	began, rolledBack := false, false
	defer func() {
		if began && !rolledBack {
			// Best effort, also when ctx is done: never leave the
			// transaction open on pgAdmin's connection.
			rctx, cancel := context.WithTimeout(context.Background(), c.env.HTTPTimeout)
			defer cancel()
			if _, rerr := c.run(rctx, trans, rollbackStmt, 0); rerr != nil && err == nil {
				err = fmt.Errorf("rollback failed: %w", rerr)
			}
		}
	}()

	for i, s := range stmts {
		if i > 0 && began && s != rollbackStmt && !strings.HasPrefix(s, "SET LOCAL ") {
			if err := c.checkReadOnly(ctx, trans); err != nil {
				return nil, err
			}
		}
		b, err := c.run(ctx, trans, s, limit)
		if err != nil {
			// errSession makes Exec run the whole batch again once, on a
			// new session; the batch is read-only, so that is safe.
			return nil, err
		}
		blocks = append(blocks, b)
		if b.Kind == adminer.KindError {
			return blocks, nil
		}
		if i == 0 {
			began = true
		}
		if s == rollbackStmt {
			rolledBack = true
		}
	}
	return blocks, nil
}

// checkReadOnly asks Postgres whether the open transaction is read-only.
func (c *Client) checkReadOnly(ctx context.Context, trans string) error {
	b, err := c.run(ctx, trans, readOnlyCheck, 1)
	if err != nil {
		return fmt.Errorf("read-only check failed: %w", err)
	}
	if b.Kind != adminer.KindTable || len(b.Rows) != 1 || len(b.Rows[0]) != 1 || b.Rows[0][0] == nil || *b.Rows[0][0] != "on" {
		return errors.New("read-only check failed: the transaction is not read-only; statement not sent")
	}
	return nil
}

// ensureLogin logs in when there is no session. A failed login is latched
// for loginCooldown.
func (c *Client) ensureLogin(ctx context.Context) error {
	if c.csrf != "" {
		return nil
	}
	if c.loginErr != nil && c.now().Sub(c.loginAt) < loginCooldown {
		return fmt.Errorf("%w (failed recently; not trying again for %s): %v", ErrLogin, loginCooldown, c.loginErr)
	}
	c.loginErr = nil
	// pgAdmin (Flask-Security) gives the CSRF token to a JSON request for
	// /login; the HTML login page does not carry it.
	page, _, err := c.request(ctx, http.MethodGet, "/login", nil, "")
	if err != nil {
		return err
	}
	var lr struct {
		Response struct {
			CSRF string `json:"csrf_token"`
		} `json:"response"`
	}
	if json.Unmarshal(page, &lr) != nil || lr.Response.CSRF == "" {
		return errors.New("pgadmin: no CSRF token in the /login answer")
	}
	tok := lr.Response.CSRF
	form := url.Values{"csrf_token": {tok}, "email": {c.env.Username}, "password": {c.password}, "language": {"en"}, "internal_button": {"Login"}}
	_, final, err := c.request(ctx, http.MethodPost, "/authenticate/login", []byte(form.Encode()), "application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(final, "/browser") {
		c.loginErr = errors.New("pgAdmin returned to the login page (wrong email or password, or the account is locked)")
		c.loginAt = c.now()
		return fmt.Errorf("%w: %v", ErrLogin, c.loginErr)
	}
	c.csrf = tok
	return nil
}

// apiResp is pgAdmin's JSON envelope. Server nodes come in result, the
// others in data.
type apiResp struct {
	Success  int             `json:"success"`
	ErrorMsg string          `json:"errormsg"`
	Info     string          `json:"info"`
	Result   json.RawMessage `json:"result"`
	Data     json.RawMessage `json:"data"`
}

type node struct {
	Label     string          `json:"label"`
	ID        json.RawMessage `json:"_id"`
	Connected bool            `json:"connected"`
	Username  string          `json:"username"`
}

func (n node) id() string { return strings.Trim(string(n.ID), `"`) }

func nodes(r *apiResp) ([]node, error) {
	raw := r.Data
	if len(raw) == 0 || string(raw) == "null" {
		raw = r.Result
	}
	var ns []node
	if err := json.Unmarshal(raw, &ns); err != nil {
		return nil, errors.New("pgadmin: unreadable node list")
	}
	return ns, nil
}

// ensureTool finds the server, connects it (its password is saved in
// pgAdmin) and opens a query tool on database, once per database.
func (c *Client) ensureTool(ctx context.Context, database string) (string, error) {
	if t, ok := c.tools[database]; ok {
		return t, nil
	}
	if c.sid == "" {
		if err := c.findServer(ctx); err != nil {
			return "", err
		}
	}
	r, err := c.api(ctx, http.MethodGet, fmt.Sprintf("/browser/database/nodes/%s/%s/", c.sgid, c.sid), nil)
	if err != nil {
		return "", err
	}
	dbs, err := nodes(r)
	if err != nil {
		return "", err
	}
	did := ""
	for _, d := range dbs {
		if d.Label == database {
			did = d.id()
		}
	}
	if did == "" {
		return "", fmt.Errorf("database %q is not on pgAdmin server %q", database, c.env.Server)
	}
	n, err := rand.Int(rand.Reader, big.NewInt(9000000))
	if err != nil {
		return "", err
	}
	trans := strconv.FormatInt(n.Int64()+1000000, 10)
	body := map[string]any{"dbname": database, "user": c.dbUser, "role": nil, "password": nil}
	if _, err := c.api(ctx, http.MethodPost, fmt.Sprintf("/sqleditor/initialize/sqleditor/%s/%s/%s/%s", trans, c.sgid, c.sid, did), body); err != nil {
		return "", fmt.Errorf("open query tool: %w", err)
	}
	c.tools[database] = trans
	return trans, nil
}

func (c *Client) findServer(ctx context.Context) error {
	r, err := c.api(ctx, http.MethodGet, "/browser/server_group/nodes/", nil)
	if err != nil {
		return err
	}
	groups, err := nodes(r)
	if err != nil {
		return err
	}
	var labels []string
	for _, g := range groups {
		r, err := c.api(ctx, http.MethodGet, "/browser/server/nodes/"+g.id(), nil)
		if err != nil {
			return err
		}
		servers, err := nodes(r)
		if err != nil {
			return err
		}
		for _, s := range servers {
			labels = append(labels, s.Label)
			if s.Label != c.env.Server {
				continue
			}
			if !s.Connected {
				if _, err := c.api(ctx, http.MethodPost, fmt.Sprintf("/browser/server/connect/%s/%s", g.id(), s.id()), map[string]any{}); err != nil {
					return fmt.Errorf("connect server %q: %w", c.env.Server, err)
				}
			}
			c.sgid, c.sid, c.dbUser = g.id(), s.id(), s.Username
			return nil
		}
	}
	return fmt.Errorf("pgAdmin has no server %q; servers: %s", c.env.Server, strings.Join(labels, ", "))
}

// pollData is the data of a finished query.
type pollData struct {
	Status       string            `json:"status"`
	Result       json.RawMessage   `json:"result"`
	ColInfo      []json.RawMessage `json:"colinfo"`
	RowsAffected int               `json:"rows_affected"`
}

// run starts sql on the query tool and waits for its result.
func (c *Client) run(ctx context.Context, trans, sql string, limit int) (adminer.Block, error) {
	b := adminer.Block{SQL: sql}
	if _, err := c.api(ctx, http.MethodPost, "/sqleditor/query_tool/start/"+trans, map[string]any{"sql": sql, "explain_plan": nil}); err != nil {
		return b, err
	}
	// statement_timeout ends the query in Postgres first; this only bounds
	// a pgAdmin that keeps answering Busy.
	deadline := c.now().Add(c.env.StatementTimeout + c.env.HTTPTimeout)
	for {
		data, status, err := c.raw(ctx, http.MethodGet, "/sqleditor/poll/"+trans, nil)
		if err != nil {
			return b, err
		}
		var r apiResp
		if json.Unmarshal(data, &r) != nil {
			return b, fmt.Errorf("pgadmin: unreadable poll response (HTTP %d)", status)
		}
		if r.Success != 1 {
			if status == http.StatusInternalServerError && r.ErrorMsg != "" {
				b.Kind, b.Message = adminer.KindError, strings.TrimSpace(r.ErrorMsg)
				return b, nil
			}
			return b, c.apiError(status, &r)
		}
		var d pollData
		dec := json.NewDecoder(bytes.NewReader(r.Data))
		dec.UseNumber()
		if err := dec.Decode(&d); err != nil {
			return b, errors.New("pgadmin: unreadable poll data")
		}
		switch d.Status {
		case "Busy":
			if c.now().After(deadline) {
				return b, errors.New("pgadmin: query still busy after the statement and HTTP timeouts")
			}
			select {
			case <-ctx.Done():
				return b, ctx.Err()
			case <-time.After(c.pollEvery):
			}
			continue
		case "Success":
		default:
			return b, fmt.Errorf("pgadmin: query status %q", d.Status)
		}
		if d.ColInfo == nil {
			var tag string
			_ = json.Unmarshal(d.Result, &tag)
			b.Kind, b.Message = adminer.KindMessage, tag
			return b, nil
		}
		return c.table(ctx, trans, b, d, limit)
	}
}

// table builds a table block and fetches the rows beyond the first page.
func (c *Client) table(ctx context.Context, trans string, b adminer.Block, d pollData, limit int) (adminer.Block, error) {
	b.Kind = adminer.KindTable
	b.Columns = make([]string, len(d.ColInfo))
	for i, raw := range d.ColInfo {
		var ci struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &ci)
		b.Columns[i] = ci.Name
	}
	want := limit
	if want <= 0 || want > maxRows {
		want = maxRows
	}
	rows, err := decodeRows(d.Result)
	if err != nil {
		return b, err
	}
	for len(rows) < want && len(rows) < d.RowsAffected {
		from := len(rows) + 1
		to := min(from+fetchPage-1, want, d.RowsAffected)
		r, err := c.api(ctx, http.MethodGet, fmt.Sprintf("/sqleditor/fetch_window/%s/%d/%d", trans, from, to), nil)
		if err != nil {
			return b, fmt.Errorf("fetch rows %d-%d: %w", from, to, err)
		}
		var page struct {
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(r.Data, &page)
		more, err := decodeRows(page.Result)
		if err != nil {
			return b, err
		}
		if len(more) == 0 {
			break
		}
		rows = append(rows, more...)
	}
	if len(rows) > want {
		rows = rows[:want]
	}
	b.Rows = rows
	return b, nil
}

// decodeRows converts pgAdmin's JSON rows to the bridge's text cells: SQL
// NULL is nil, numbers keep their JSON text, booleans are true/false, JSON
// values are compact JSON.
//
// A result without rows is null or the command tag ("SELECT 0").
func decodeRows(raw json.RawMessage) ([][]*string, error) {
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] == '"' || string(t) == "null" {
		return [][]*string{}, nil
	}
	var rows [][]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&rows); err != nil {
		return nil, errors.New("pgadmin: unreadable rows")
	}
	out := make([][]*string, len(rows))
	for i, row := range rows {
		out[i] = make([]*string, len(row))
		for j, v := range row {
			var s string
			switch x := v.(type) {
			case nil:
				continue
			case string:
				s = x
			case json.Number:
				s = x.String()
			case bool:
				s = strconv.FormatBool(x)
			default:
				bs, err := json.Marshal(x)
				if err != nil {
					return nil, errors.New("pgadmin: unreadable cell")
				}
				s = string(bs)
			}
			out[i][j] = &s
		}
	}
	return out, nil
}

// api sends a JSON request and returns the envelope of a successful answer.
func (c *Client) api(ctx context.Context, method, path string, body any) (*apiResp, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	data, status, err := c.raw(ctx, method, path, payload)
	if err != nil {
		return nil, err
	}
	var r apiResp
	if json.Unmarshal(data, &r) != nil {
		if status == http.StatusUnauthorized {
			return nil, errSession
		}
		return nil, fmt.Errorf("pgadmin: unreadable response (HTTP %d)", status)
	}
	if status != http.StatusOK || r.Success != 1 {
		return nil, c.apiError(status, &r)
	}
	return &r, nil
}

func (c *Client) apiError(status int, r *apiResp) error {
	if status == http.StatusUnauthorized || r.Info == "DATAGRID_TRANSACTION_REQUIRED" {
		return errSession
	}
	return fmt.Errorf("pgadmin: HTTP %d: %s", status, c.scrub(strings.TrimSpace(r.ErrorMsg)))
}

// raw sends an API request with the session's CSRF header.
func (c *Client) raw(ctx context.Context, method, path string, payload []byte) ([]byte, int, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	// pgAdmin checks the referrer strictly over HTTPS and answers 401
	// without it.
	req.Header.Set("Referer", c.base+"/browser/")
	req.Header.Set("X-pgA-CSRFToken", c.csrf)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, c.transportError(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, 0, c.transportError(err)
	}
	// An expired session is redirected to the login page.
	if strings.HasPrefix(resp.Request.URL.Path, "/login") {
		return nil, 0, errSession
	}
	return data, resp.StatusCode, nil
}

// request is for the login pages: it returns the body and the final path
// after redirects.
func (c *Client) request(ctx context.Context, method, path string, body []byte, ctype string) ([]byte, string, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", c.base+"/login")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", c.transportError(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, "", c.transportError(err)
	}
	return data, resp.Request.URL.Path, nil
}

func (c *Client) transportError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("pgadmin: request failed: %s", c.scrub(err.Error()))
}

func (c *Client) scrub(s string) string {
	if c.password == "" {
		return s
	}
	return strings.ReplaceAll(s, c.password, "[redacted]")
}
