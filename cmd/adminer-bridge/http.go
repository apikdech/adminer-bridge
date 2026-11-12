package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultTokenEnv = "ADMINER_BRIDGE_TOKEN"
	minTokenLen     = 32
	maxBodyBytes    = 1 << 20 // 1 MiB
	// idleSessionTimeout closes MCP sessions that send no request for this
	// long, so sessions of finished agent runs do not pile up.
	idleSessionTimeout = time.Hour
	shutdownTimeout    = 5 * time.Second
)

// tokenFromEnv reads the bearer token from the named variable. Errors name
// the variable and never contain its value.
func tokenFromEnv(name string) (string, error) {
	tok := os.Getenv(name)
	if tok == "" {
		return "", fmt.Errorf("HTTP mode needs a bearer token: set %s (for example: openssl rand -hex 32)", name)
	}
	if len(tok) < minTokenLen {
		return "", fmt.Errorf("the token in %s is too short: it must have at least %d characters (for example: openssl rand -hex 32)", name, minTokenLen)
	}
	return tok, nil
}

// newHTTPHandler routes /mcp (bearer auth, streamable HTTP) and /healthz
// (no auth). Every other path is 404. All clients share srv.
func newHTTPHandler(srv *mcp.Server, token string, logger *log.Logger) http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		MaxRequestBodyBytes: maxBodyBytes,
		SessionTimeout:      idleSessionTimeout,
	})
	want := sha256.Sum256([]byte(token))
	authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(got))
		if !ok || subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			// Never log the presented token.
			logger.Printf("unauthorized request: %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})

	mux := http.NewServeMux()
	mux.Handle("/mcp", authed)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "ok\n")
	})
	return mux
}

// serveHTTP listens on addr until ctx is done, then shuts down gracefully.
//
// WriteTimeout is unset on purpose: a tool call can take up to the longest
// environment http_timeout, and the standalone SSE stream (GET /mcp) stays
// open for the whole session. A write timeout would cut both.
func serveHTTP(ctx context.Context, addr string, h http.Handler, logger *log.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	hs := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          logger,
	}
	logger.Printf("listening on %s (MCP at /mcp, health at /healthz)", ln.Addr())
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	logger.Printf("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil {
		// Long-lived SSE streams keep connections open; close them.
		hs.Close()
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
