package adminer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"adminer-bridge/config"
)

// ErrLogin means Adminer rejected the login. It is never retried: Adminer
// throttles IPs after failed logins.
var ErrLogin = errors.New("adminer login failed")

const maxPageBytes = 64 << 20

// Client holds one Adminer session (cookie jar and CSRF token) for an environment.
type Client struct {
	env      *config.Env
	password string
	http     *http.Client
	mu       sync.Mutex
	token    string

	loggedIn bool      // a login POST has succeeded on this client
	loginErr error     // latched ErrLogin, see loginCooldown
	loginAt  time.Time // when loginErr was latched
	now      func() time.Time
}

// loginCooldown is how long a failed login is remembered. Exec calls inside
// it return the cached error without sending credentials again, protecting
// Adminer's failed-login throttle from a caller that retries.
const loginCooldown = 60 * time.Second

// NewClient prepares a client for env. It reads the password from the
// environment variable named by env.PasswordEnv but makes no requests.
func NewClient(env *config.Env) (*Client, error) {
	pw, err := env.Password()
	if err != nil {
		return nil, fmt.Errorf("environment %q: %w", env.Name, err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("environment %q: %w", env.Name, err)
	}
	return &Client{
		env:      env,
		password: pw,
		http:     &http.Client{Jar: jar, Timeout: env.HTTPTimeout},
		now:      time.Now,
	}, nil
}

func (c *Client) sqlURL() string {
	q := url.Values{}
	q.Set("pgsql", c.env.Server)
	q.Set("username", c.env.Username)
	q.Set("db", c.env.Database)
	q.Set("ns", c.env.Schema)
	q.Set("sql", "")
	sep := "?"
	if strings.Contains(c.env.URL, "?") {
		sep = "&"
	}
	return c.env.URL + sep + q.Encode()
}

// page is a parsed Adminer response.
type page struct {
	kind    PageKind
	blocks  []Block
	token   string
	pageErr string
	status  int
}

func (c *Client) do(req *http.Request) (*page, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("environment %q: adminer request failed: %w", c.env.Name, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("environment %q: read adminer response: %w", c.env.Name, err)
	}
	if len(body) > maxPageBytes {
		return nil, fmt.Errorf("environment %q: adminer response exceeds %d bytes", c.env.Name, maxPageBytes)
	}
	kind, blocks, token, pageErr, err := ParsePage(body)
	if err != nil {
		return nil, fmt.Errorf("environment %q: parse adminer response: %w", c.env.Name, err)
	}
	if kind == PageSQL && token != "" {
		c.token = token
	}
	return &page{kind, blocks, token, pageErr, resp.StatusCode}, nil
}

func (c *Client) get(ctx context.Context) (*page, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sqlURL(), nil)
	if err != nil {
		return nil, fmt.Errorf("environment %q: %w", c.env.Name, err)
	}
	return c.do(req)
}

// login posts credentials once. A login page in the answer is ErrLogin.
func (c *Client) login(ctx context.Context) error {
	if c.loginErr != nil && c.now().Sub(c.loginAt) < loginCooldown {
		return fmt.Errorf("environment %q: %w (login failed recently; retry after the %s cooldown, not sending credentials again): %v",
			c.env.Name, ErrLogin, loginCooldown, c.loginErr)
	}
	err := c.doLogin(ctx)
	if errors.Is(err, ErrLogin) {
		c.loginErr, c.loginAt = err, c.now()
	} else if err == nil {
		c.loginErr, c.loggedIn = nil, true
	}
	return err
}

func (c *Client) doLogin(ctx context.Context) error {
	form := url.Values{
		"auth[driver]":   {"pgsql"},
		"auth[server]":   {c.env.Server},
		"auth[username]": {c.env.Username},
		"auth[password]": {c.password},
		"auth[db]":       {c.env.Database},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.env.URL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("environment %q: %w", c.env.Name, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	p, err := c.do(req)
	if err != nil {
		return err
	}
	if p.kind == PageLogin {
		return fmt.Errorf("environment %q: %w%s", c.env.Name, ErrLogin, suffix(p.pageErr))
	}
	return nil
}

// fetchToken GETs the SQL page and caches its CSRF token. It reports whether
// the page was the login page (no session).
func (c *Client) fetchToken(ctx context.Context) (loginPage bool, err error) {
	c.token = ""
	p, err := c.get(ctx)
	if err != nil {
		return false, err
	}
	if p.kind == PageLogin {
		return true, nil
	}
	if c.token == "" {
		return false, fmt.Errorf("environment %q: no CSRF token on adminer SQL page", c.env.Name)
	}
	return false, nil
}

// refreshToken fetches a CSRF token, logging in first when asked to. A login
// page after that is ErrLogin: the login did not take.
func (c *Client) refreshToken(ctx context.Context, doLogin bool) error {
	if doLogin {
		if err := c.login(ctx); err != nil {
			return err
		}
	}
	isLogin, err := c.fetchToken(ctx)
	if err != nil {
		return err
	}
	if isLogin {
		return fmt.Errorf("environment %q: %w: adminer session not established", c.env.Name, ErrLogin)
	}
	return nil
}

func suffix(msg string) string {
	if msg == "" {
		return ""
	}
	return ": " + msg
}

func (c *Client) post(ctx context.Context, batch string, limit int) (*page, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	limitStr := ""
	if limit > 0 {
		limitStr = strconv.Itoa(limit)
	}
	for _, f := range [][2]string{
		{"query", batch}, {"limit", limitStr}, {"error_stops", "1"}, {"token", c.token},
	} {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			return nil, fmt.Errorf("environment %q: %w", c.env.Name, err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("environment %q: %w", c.env.Name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.sqlURL(), &buf)
	if err != nil {
		return nil, fmt.Errorf("environment %q: %w", c.env.Name, err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return c.do(req)
}

// Exec runs batch (limit 0 = unlimited) and returns one Block per statement.
// It logs in when needed, and retries once, only for an expired session or an
// invalid CSRF token.
func (c *Client) Exec(ctx context.Context, batch string, limit int) ([]Block, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token == "" {
		// Without a session, log in directly: a request carrying username=
		// but no session cookie counts as a failed login to Adminer's throttle.
		if err := c.refreshToken(ctx, !c.loggedIn); err != nil {
			return nil, err
		}
	}
	retried := false
	for {
		p, err := c.post(ctx, batch, limit)
		if err != nil {
			return nil, err
		}
		expired := p.kind == PageLogin
		badToken := p.kind == PageSQL && strings.Contains(p.pageErr, "Invalid CSRF token")
		if !expired && !badToken {
			if p.pageErr != "" {
				return nil, fmt.Errorf("environment %q: adminer error (HTTP %d): %s", c.env.Name, p.status, p.pageErr)
			}
			if len(p.blocks) == 0 {
				return nil, fmt.Errorf("environment %q: unexpected adminer response (HTTP %d)", c.env.Name, p.status)
			}
			return p.blocks, nil
		}
		reason := "session expired"
		if badToken {
			reason = "invalid CSRF token"
		}
		if retried {
			return nil, fmt.Errorf("environment %q: adminer %s again after retry", c.env.Name, reason)
		}
		retried = true
		c.token = ""
		if err := c.refreshToken(ctx, expired); err != nil {
			return nil, err
		}
	}
}
