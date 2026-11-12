package adminer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"adminer-bridge/config"
)

const testPassword = "s3cret-pw-value"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type recorded struct {
	method string
	query  url.Values
	form   map[string]string
}

// fakeAdminer serves fixtures and records requests. batchPages are served for
// successive batch POSTs (the last repeats); loginPage is what a login POST answers.
type fakeAdminer struct {
	t          *testing.T
	mu         sync.Mutex
	loggedIn   bool
	loginPage  string // fixture served after login POST; "" = sql_page.html
	batchPages []string
	reqs       []recorded
	logins     int
	batches    []recorded
	events     []string          // "login" or "GET ?query" in arrival order
	failGets   int               // the next failGets GETs answer 502 (no SQL page)
	dbPages    map[string]string // fixture for any request whose db= is a key (Adminer cannot open that database)
}

func (f *fakeAdminer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := recorded{method: r.Method, query: r.URL.Query(), form: map[string]string{}}
	if r.Method == http.MethodPost {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			if err := r.ParseForm(); err != nil {
				f.t.Errorf("parse form: %v", err)
			}
		}
		for k, v := range r.Form {
			rec.form[k] = v[0]
		}
	}
	f.reqs = append(f.reqs, rec)
	serve := func(name string) {
		// "raw:<status>:<body>" serves an inline non-fixture response.
		if rest, ok := strings.CutPrefix(name, "raw:"); ok {
			code, body, _ := strings.Cut(rest, ":")
			n, _ := strconv.Atoi(code)
			w.WriteHeader(n)
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write(fixture(f.t, name))
	}
	if r.Method == http.MethodPost && strings.Contains(r.Header.Get("Content-Type"), "urlencoded") {
		f.events = append(f.events, "login")
	} else {
		f.events = append(f.events, r.Method+" "+r.URL.RawQuery)
	}

	if page, ok := f.dbPages[r.URL.Query().Get("db")]; ok && r.URL.Query().Has("sql") {
		if r.Method == http.MethodPost {
			f.batches = append(f.batches, rec)
		}
		serve(page)
		return
	}

	switch {
	case r.Method == http.MethodPost && r.PostForm.Has("auth[password]"):
		f.logins++
		if rec.form["auth[password]"] != testPassword {
			f.t.Errorf("login password wrong")
		}
		if f.loginPage != "" {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		f.loggedIn = true
		http.Redirect(w, r, "/", http.StatusFound)
	case r.Method == http.MethodPost:
		f.batches = append(f.batches, rec)
		i := len(f.batches) - 1
		if i >= len(f.batchPages) {
			i = len(f.batchPages) - 1
		}
		serve(f.batchPages[i])
	default:
		if f.failGets > 0 {
			f.failGets--
			serve("raw:502:<html><body>Bad Gateway</body></html>")
		} else if f.loginPage != "" {
			serve(f.loginPage)
		} else if f.loggedIn {
			serve("sql_page.html")
		} else {
			serve("login.html")
		}
	}
}

func newTestClient(t *testing.T, f *fakeAdminer) (*Client, *httptest.Server) {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("TEST_ADMINER_PW", testPassword)
	c, err := NewClient(&config.Env{
		Name: "staging", URL: srv.URL + "/", Server: "database", Username: "postgres",
		Database: "glchat", Schema: "public", PasswordEnv: "TEST_ADMINER_PW",
		HTTPTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestExecLogsInThenRuns(t *testing.T) {
	f := &fakeAdminer{batchPages: []string{"ok.html"}}
	c, _ := newTestClient(t, f)
	batch := "SELECT 1; SELECT 'a;b'"
	blocks, err := c.Exec(context.Background(), "glchat", batch, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 6 {
		t.Fatalf("blocks = %d, want 6", len(blocks))
	}
	if f.logins != 1 {
		t.Errorf("logins = %d, want 1", f.logins)
	}
	// No request carrying username= may precede the first login: Adminer
	// counts it as a failed login toward its throttle.
	for _, e := range f.events {
		if e == "login" {
			break
		}
		if strings.Contains(e, "username=") {
			t.Errorf("request before first login carried username=: %q", e)
		}
	}
	if f.events[0] != "login" {
		t.Errorf("first request = %q, want login", f.events[0])
	}
	_, _, wantTok, _, _ := ParsePage(fixture(t, "sql_page.html"))
	b := f.batches[0].form
	if b["token"] != wantTok || wantTok == "" {
		t.Errorf("token = %q, want %q", b["token"], wantTok)
	}
	if b["error_stops"] != "1" {
		t.Errorf("error_stops = %q", b["error_stops"])
	}
	if b["query"] != batch {
		t.Errorf("query = %q, want %q", b["query"], batch)
	}
}

func TestExecReloginOnceOnExpiredSession(t *testing.T) {
	f := &fakeAdminer{batchPages: []string{"login.html", "ok.html"}}
	c, _ := newTestClient(t, f)
	if _, err := c.Exec(context.Background(), "glchat", "SELECT 1", 0); err != nil {
		t.Fatal(err)
	}
	if f.logins != 2 {
		t.Errorf("logins = %d, want 2", f.logins)
	}
}

func TestExecFailsAfterSecondLoginPage(t *testing.T) {
	f := &fakeAdminer{batchPages: []string{"login.html"}}
	c, _ := newTestClient(t, f)
	_, err := c.Exec(context.Background(), "glchat", "SELECT 1", 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if f.logins != 2 {
		t.Errorf("logins = %d, want 2", f.logins)
	}
	if len(f.batches) != 2 {
		t.Errorf("batch posts = %d, want 2", len(f.batches))
	}
	if !strings.Contains(err.Error(), "staging") {
		t.Errorf("error lacks env name: %v", err)
	}
}

// Note: login_failed.html says "Session expired" (it was captured without the
// session cookie); real wrong-password text is "Invalid credentials." (see
// testdata/NOTES.md). The client keys on the login page, not the text.
func TestLoginFailureNoRetry(t *testing.T) {
	f := &fakeAdminer{loginPage: "login_failed.html", batchPages: []string{"ok.html"}}
	c, _ := newTestClient(t, f)
	_, err := c.Exec(context.Background(), "glchat", "SELECT 1", 0)
	if !errors.Is(err, ErrLogin) {
		t.Fatalf("err = %v, want ErrLogin", err)
	}
	if f.logins != 1 {
		t.Errorf("logins = %d, want 1", f.logins)
	}
	if !strings.Contains(err.Error(), "staging") {
		t.Errorf("error lacks env name: %v", err)
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Errorf("error leaks password: %v", err)
	}
	if !strings.Contains(err.Error(), "Session expired") {
		t.Errorf("error lacks Adminer message: %v", err)
	}
	// A later Exec attempts login again only because the caller asked; no
	// batch must ever have been sent.
	if len(f.batches) != 0 {
		t.Errorf("batches sent: %d", len(f.batches))
	}
}

func TestExecRetriesOnceOnBadToken(t *testing.T) {
	f := &fakeAdminer{batchPages: []string{"bad_token.html", "ok.html"}}
	c, _ := newTestClient(t, f)
	blocks, err := c.Exec(context.Background(), "glchat", "SELECT 1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 6 {
		t.Errorf("blocks = %d", len(blocks))
	}
	if len(f.batches) != 2 {
		t.Errorf("batch posts = %d, want 2", len(f.batches))
	}
	if f.batches[0].form["token"] == "" || f.batches[1].form["token"] == "" {
		t.Errorf("missing token on a batch post")
	}
	// The retry must follow a token refetch: a SQL page GET between the posts.
	posts, gets := 0, 0
	for _, e := range f.events {
		if strings.HasPrefix(e, "POST") {
			posts++
		} else if strings.HasPrefix(e, "GET") && posts == 1 {
			gets++
		}
	}
	if gets != 1 {
		t.Errorf("GETs between batch posts = %d, want 1", gets)
	}
	if f.logins != 1 {
		t.Errorf("logins = %d, want 1", f.logins)
	}
}

func TestExecBadTokenTwiceFails(t *testing.T) {
	f := &fakeAdminer{batchPages: []string{"bad_token.html"}}
	c, _ := newTestClient(t, f)
	_, err := c.Exec(context.Background(), "glchat", "SELECT 1", 0)
	if err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("err = %v", err)
	}
	if len(f.batches) != 2 {
		t.Errorf("batch posts = %d, want 2", len(f.batches))
	}
}

func TestLimitField(t *testing.T) {
	f := &fakeAdminer{batchPages: []string{"ok.html"}}
	c, _ := newTestClient(t, f)
	if _, err := c.Exec(context.Background(), "glchat", "SELECT 1", 101); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(context.Background(), "glchat", "SELECT 1", 0); err != nil {
		t.Fatal(err)
	}
	if got := f.batches[0].form["limit"]; got != "101" {
		t.Errorf("limit = %q, want 101", got)
	}
	if got, ok := f.batches[1].form["limit"]; !ok || got != "" {
		t.Errorf("limit = %q (present=%v), want empty", got, ok)
	}
	if f.logins != 1 {
		t.Errorf("logins = %d, want 1 (session reuse)", f.logins)
	}
}

func TestUnreachable(t *testing.T) {
	f := &fakeAdminer{batchPages: []string{"ok.html"}}
	c, srv := newTestClient(t, f)
	srv.Close()
	_, err := c.Exec(context.Background(), "glchat", "SELECT 1", 0)
	if err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Errorf("error leaks password")
	}
}

func TestUnexpectedPagesFail(t *testing.T) {
	cases := map[string]struct{ page, want string }{
		"proxy 502": {"raw:502:<html><body>Bad Gateway</body></html>", "HTTP 502"},
		"empty":     {"raw:200:", "unexpected adminer response"},
		"other div.error": {"raw:200:<div id='content'><div class='error'>Too many unsuccessful logins, try again in 29 minute(s).</div></div>",
			"Too many unsuccessful logins"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeAdminer{batchPages: []string{tc.page}}
			c, _ := newTestClient(t, f)
			blocks, err := c.Exec(context.Background(), "glchat", "SELECT 1", 0)
			if err == nil {
				t.Fatalf("expected error, got blocks %v", blocks)
			}
			if !strings.Contains(err.Error(), "staging") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want env name and %q", err, tc.want)
			}
		})
	}
}

func TestLoginFailureLatchedForCooldown(t *testing.T) {
	f := &fakeAdminer{loginPage: "login_failed.html", batchPages: []string{"ok.html"}}
	c, _ := newTestClient(t, f)
	now := time.Now()
	c.now = func() time.Time { return now }
	if _, err := c.Exec(context.Background(), "glchat", "SELECT 1", 0); !errors.Is(err, ErrLogin) {
		t.Fatalf("err = %v", err)
	}
	now = now.Add(30 * time.Second)
	_, err := c.Exec(context.Background(), "glchat", "SELECT 1", 0)
	if !errors.Is(err, ErrLogin) || !strings.Contains(err.Error(), "staging") || !strings.Contains(err.Error(), "cooldown") {
		t.Fatalf("err = %v", err)
	}
	if f.logins != 1 {
		t.Fatalf("logins = %d within cooldown, want 1", f.logins)
	}
	now = now.Add(31 * time.Second)
	f.mu.Lock()
	f.loginPage = ""
	f.mu.Unlock()
	if _, err := c.Exec(context.Background(), "glchat", "SELECT 1", 0); err != nil {
		t.Fatalf("after cooldown: %v", err)
	}
	if f.logins != 2 {
		t.Errorf("logins = %d after cooldown, want 2", f.logins)
	}
}

// stuckClient leaves c with no CSRF token after a successful login: the
// token refetch after an invalid-CSRF answer fails, then Adminer's session
// expires. This is the state that used to fail every later Exec.
func stuckClient(t *testing.T) (*Client, *fakeAdminer) {
	t.Helper()
	f := &fakeAdminer{batchPages: []string{"ok.html", "bad_token.html", "ok.html"}}
	c, _ := newTestClient(t, f)
	if _, err := c.Exec(context.Background(), "glchat", "SELECT 1", 0); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.failGets = 1
	f.mu.Unlock()
	if _, err := c.Exec(context.Background(), "glchat", "SELECT 1", 0); err == nil {
		t.Fatal("token refetch should have failed")
	}
	f.mu.Lock()
	f.loggedIn = false // the Adminer session expires
	f.mu.Unlock()
	if f.logins != 1 {
		t.Fatalf("setup logins = %d, want 1", f.logins)
	}
	return c, f
}

func TestExecRecoversWhenTokenFetchFindsLoginPage(t *testing.T) {
	c, f := stuckClient(t)
	for i := range 3 {
		if _, err := c.Exec(context.Background(), "glchat", "SELECT 1", 0); err != nil {
			t.Fatalf("Exec %d: %v", i, err)
		}
	}
	if f.logins != 2 {
		t.Errorf("logins = %d, want 2 (one re-login, then session reuse)", f.logins)
	}
}

func TestExecRecoveryLoginFailureNotRetried(t *testing.T) {
	c, f := stuckClient(t)
	f.mu.Lock()
	f.loginPage = "login_failed.html"
	f.mu.Unlock()
	if _, err := c.Exec(context.Background(), "glchat", "SELECT 1", 0); !errors.Is(err, ErrLogin) {
		t.Fatalf("err = %v, want ErrLogin", err)
	}
	if _, err := c.Exec(context.Background(), "glchat", "SELECT 1", 0); !errors.Is(err, ErrLogin) || !strings.Contains(err.Error(), "cooldown") {
		t.Fatalf("second err = %v, want latched ErrLogin", err)
	}
	if f.logins != 2 {
		t.Errorf("logins = %d, want 2 (one recovery login, never retried)", f.logins)
	}
}

func TestExecUsesRequestedDatabaseWithOneLogin(t *testing.T) {
	f := &fakeAdminer{batchPages: []string{"ok.html"}}
	c, _ := newTestClient(t, f)
	for _, db := range []string{"glchat", "postgres", "glchat"} {
		if _, err := c.Exec(context.Background(), db, "SELECT 1", 0); err != nil {
			t.Fatalf("Exec(%s): %v", db, err)
		}
	}
	if f.logins != 1 {
		t.Errorf("logins = %d, want 1 (one session for all databases)", f.logins)
	}
	if got := f.reqs[0].form["auth[db]"]; got != "glchat" {
		t.Errorf("login auth[db] = %q, want the default database glchat", got)
	}
	if len(f.batches) != 3 {
		t.Fatalf("batch posts = %d, want 3", len(f.batches))
	}
	for i, want := range []string{"glchat", "postgres", "glchat"} {
		q := f.batches[i].query
		if got := q.Get("db"); got != want {
			t.Errorf("batch %d db = %q, want %q", i, got, want)
		}
		if q.Get("pgsql") != "database" || q.Get("username") != "postgres" || q.Get("ns") != "public" || !q.Has("sql") {
			t.Errorf("batch %d query = %v", i, q)
		}
	}
}

// Token fetches always use the default database: a token is per session, and
// a request for a database Adminer cannot open costs a failed-login strike.
func TestExecTokenFetchUsesDefaultDatabase(t *testing.T) {
	f := &fakeAdminer{batchPages: []string{"ok.html"}}
	c, _ := newTestClient(t, f)
	if _, err := c.Exec(context.Background(), "postgres", "SELECT 1", 0); err != nil {
		t.Fatal(err)
	}
	var gets []url.Values
	for _, r := range f.reqs {
		if r.method == http.MethodGet && r.query.Has("sql") { // skip the login redirect
			gets = append(gets, r.query)
		}
	}
	if len(gets) != 1 || gets[0].Get("db") != "glchat" {
		t.Fatalf("token GETs = %v, want one with db=glchat", gets)
	}
	if got := f.batches[0].query.Get("db"); got != "postgres" {
		t.Errorf("batch db = %q, want postgres", got)
	}
}

func TestExecNonDefaultDatabaseLoginPageNoRelogin(t *testing.T) {
	f := &fakeAdminer{batchPages: []string{"ok.html"}, dbPages: map[string]string{"broken": "login_failed.html"}}
	c, _ := newTestClient(t, f)
	now := time.Now()
	c.now = func() time.Time { return now }
	ctx := context.Background()

	_, err := c.Exec(ctx, "broken", "SELECT 1", 0)
	if !errors.Is(err, ErrDatabase) || errors.Is(err, ErrLogin) {
		t.Fatalf("err = %v, want ErrDatabase and not ErrLogin", err)
	}
	for _, want := range []string{`"staging"`, `"broken"`, "Session expired"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %s", err, want)
		}
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Errorf("error leaks password")
	}
	if f.logins != 1 {
		t.Errorf("logins = %d, want 1 (no re-login for a database failure)", f.logins)
	}
	if len(f.batches) != 1 {
		t.Errorf("batch posts = %d, want 1 (no resend)", len(f.batches))
	}

	// Latched: no request at all inside the cooldown.
	n := len(f.reqs)
	now = now.Add(59 * time.Second)
	_, err = c.Exec(ctx, "broken", "SELECT 1", 0)
	if !errors.Is(err, ErrDatabase) || !strings.Contains(err.Error(), `"broken"`) || !strings.Contains(err.Error(), "cooldown") {
		t.Fatalf("latched err = %v", err)
	}
	if len(f.reqs) != n {
		t.Fatalf("requests during cooldown = %d, want 0", len(f.reqs)-n)
	}

	// The default database is not latched.
	if _, err := c.Exec(ctx, "glchat", "SELECT 1", 0); err != nil {
		t.Fatalf("default database: %v", err)
	}

	// After the cooldown the database is tried again.
	now = now.Add(2 * time.Second)
	n = len(f.batches)
	if _, err := c.Exec(ctx, "broken", "SELECT 1", 0); !errors.Is(err, ErrDatabase) {
		t.Fatalf("after cooldown err = %v", err)
	}
	if len(f.batches) != n+1 {
		t.Errorf("batch posts after cooldown = %d, want 1", len(f.batches)-n)
	}
	if f.logins != 1 {
		t.Errorf("logins = %d, want 1", f.logins)
	}
}

func TestExecNonDefaultDatabaseFailureClearsToken(t *testing.T) {
	f := &fakeAdminer{batchPages: []string{"ok.html"}, dbPages: map[string]string{"broken": "login_failed.html"}}
	c, _ := newTestClient(t, f)
	ctx := context.Background()
	if _, err := c.Exec(ctx, "glchat", "SELECT 1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "broken", "SELECT 1", 0); !errors.Is(err, ErrDatabase) {
		t.Fatalf("err = %v", err)
	}
	// Adminer restarts the session on a database failure, so the next Exec
	// must fetch a new token first.
	gets := 0
	for _, e := range f.events {
		if strings.HasPrefix(e, "GET") && strings.Contains(e, "sql=") {
			gets++
		}
	}
	if _, err := c.Exec(ctx, "glchat", "SELECT 1", 0); err != nil {
		t.Fatal(err)
	}
	after := 0
	for _, e := range f.events {
		if strings.HasPrefix(e, "GET") && strings.Contains(e, "sql=") {
			after++
		}
	}
	if after != gets+1 {
		t.Errorf("token GETs after the failure = %d, want 1", after-gets)
	}
}

func TestExecRejectsEmptyDatabase(t *testing.T) {
	f := &fakeAdminer{batchPages: []string{"ok.html"}}
	c, _ := newTestClient(t, f)
	_, err := c.Exec(context.Background(), "", "SELECT 1", 0)
	if err == nil || !strings.Contains(err.Error(), "staging") || !strings.Contains(err.Error(), "database") {
		t.Fatalf("err = %v, want an error naming the environment and the database", err)
	}
	if len(f.reqs) != 0 {
		t.Errorf("requests sent: %d, want 0", len(f.reqs))
	}
}
