package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"
)

const defaultHealthURL = "http://127.0.0.1:8090/healthz"

// health checks a running HTTP server. The runtime image has no curl, so
// the Docker HEALTHCHECK uses this subcommand.
func health(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", defaultHealthURL, "health URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(*url)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check failed: status %d", resp.StatusCode)
	}
	return nil
}
