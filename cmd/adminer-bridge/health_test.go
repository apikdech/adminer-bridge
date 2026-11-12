package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthCommand(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close()

	var out, errOut bytes.Buffer
	if err := run([]string{"health", "--url", ok.URL}, &out, &errOut); err != nil {
		t.Fatalf("200: err = %v", err)
	}
	if err := run([]string{"health", "--url", bad.URL}, &out, &errOut); err == nil {
		t.Fatal("503: want an error")
	}
	if err := run([]string{"health", "--url", goneURL}, &out, &errOut); err == nil {
		t.Fatal("unreachable: want an error")
	}
}

func TestUsageMentionsHealth(t *testing.T) {
	var out, errOut bytes.Buffer
	_ = run([]string{"help"}, &out, &errOut)
	if !strings.Contains(errOut.String(), "adminer-bridge health [--url URL]") {
		t.Fatalf("usage = %q", errOut.String())
	}
}
