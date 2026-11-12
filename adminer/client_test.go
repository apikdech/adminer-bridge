package adminer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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
}

func (f *fakeAdminer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := recorded{method: r.Method, form: map[string]string{}}
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
	serve := func(name string) { _, _ = w.Write(fixture(f.t, name)) }

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
		if f.loginPage != "" {
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
	blocks, err := c.Exec(context.Background(), batch, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 6 {
		t.Fatalf("blocks = %d, want 6", len(blocks))
	}
	if f.logins != 1 {
		t.Errorf("logins = %d, want 1", f.logins)
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
	if _, err := c.Exec(context.Background(), "SELECT 1", 0); err != nil {
		t.Fatal(err)
	}
	if f.logins != 2 {
		t.Errorf("logins = %d, want 2", f.logins)
	}
}

func TestExecFailsAfterSecondLoginPage(t *testing.T) {
	f := &fakeAdminer{batchPages: []string{"login.html"}}
	c, _ := newTestClient(t, f)
	_, err := c.Exec(context.Background(), "SELECT 1", 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if f.logins > 2 {
		t.Errorf("logins = %d, want <= 2", f.logins)
	}
	if len(f.batches) != 2 {
		t.Errorf("batch posts = %d, want 2", len(f.batches))
	}
	if !strings.Contains(err.Error(), "staging") {
		t.Errorf("error lacks env name: %v", err)
	}
}

func TestLoginFailureNoRetry(t *testing.T) {
	f := &fakeAdminer{loginPage: "login_failed.html", batchPages: []string{"ok.html"}}
	c, _ := newTestClient(t, f)
	_, err := c.Exec(context.Background(), "SELECT 1", 0)
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
	blocks, err := c.Exec(context.Background(), "SELECT 1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 6 {
		t.Errorf("blocks = %d", len(blocks))
	}
	if len(f.batches) != 2 {
		t.Errorf("batch posts = %d, want 2", len(f.batches))
	}
	if f.logins != 1 {
		t.Errorf("logins = %d, want 1", f.logins)
	}
}

func TestExecBadTokenTwiceFails(t *testing.T) {
	f := &fakeAdminer{batchPages: []string{"bad_token.html"}}
	c, _ := newTestClient(t, f)
	_, err := c.Exec(context.Background(), "SELECT 1", 0)
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
	if _, err := c.Exec(context.Background(), "SELECT 1", 101); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(context.Background(), "SELECT 1", 0); err != nil {
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
	_, err := c.Exec(context.Background(), "SELECT 1", 0)
	if err == nil || !strings.Contains(err.Error(), "staging") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Errorf("error leaks password")
	}
}
