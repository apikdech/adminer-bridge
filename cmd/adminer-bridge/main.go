// Command adminer-bridge runs read-only SQL against Postgres through Adminer,
// as an MCP server (serve: stdio, or streamable HTTP with --http) or as a
// one-shot CLI (query).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/apikdech/adminer-bridge/bridge"
	"github.com/apikdech/adminer-bridge/config"
	"github.com/apikdech/adminer-bridge/mcpserver"
	"github.com/apikdech/adminer-bridge/secrets"
)

var version = "dev"

const usage = `usage:
  adminer-bridge serve [--config path] [--secrets path]
  adminer-bridge serve --http ADDR [--config path] [--secrets path] [--token-env NAME]
  adminer-bridge health [--url URL]
  adminer-bridge query [--config path] [--secrets path] --env NAME [--db NAME] [--max-rows N] [--json] "SQL"
`

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "adminer-bridge:", err)
		}
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errors.New("missing subcommand")
	}
	switch args[0] {
	case "serve":
		return serve(args[1:], stderr)
	case "query":
		return query(args[1:], stdout, stderr)
	case "health":
		return health(args[1:], stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stderr, usage)
		return flag.ErrHelp
	}
	fmt.Fprint(stderr, usage)
	return fmt.Errorf("unknown subcommand %q", args[0])
}

// httpToken loads the secrets file (when given) and then reads the bearer
// token, so the token can come from the secrets file.
func httpToken(secretsPath, tokenEnv string) (string, error) {
	if secretsPath != "" {
		if err := secrets.Load(secretsPath); err != nil {
			return "", err
		}
	}
	return tokenFromEnv(tokenEnv)
}

// loadConfig loads the secrets file first (when given), so that the
// password_env variables of the config can come from it.
func loadConfig(cfgPath, secretsPath string) (*config.Config, error) {
	if secretsPath != "" {
		if err := secrets.Load(secretsPath); err != nil {
			return nil, err
		}
	}
	if cfgPath == "" {
		cfgPath = config.DefaultPath()
	}
	return config.Load(cfgPath)
}

func loadBridge(cfgPath, secretsPath string) (*bridge.Bridge, error) {
	cfg, err := loadConfig(cfgPath, secretsPath)
	if err != nil {
		return nil, err
	}
	return bridge.New(cfg)
}

func serve(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config file (default: $ADMINER_BRIDGE_CONFIG or ~/.config/adminer-bridge/config.yaml)")
	secretsPath := fs.String("secrets", "", "KEY=VALUE file (chmod 600) loaded into the environment before the config; variables already set win")
	httpAddr := fs.String("http", "", "serve MCP over streamable HTTP at /mcp on this address (e.g. :8090) instead of stdio")
	tokenEnv := fs.String("token-env", defaultTokenEnv, "with --http: environment variable that holds the bearer token (at least 32 characters)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *httpAddr == "" {
		b, err := loadBridge(*cfgPath, *secretsPath)
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		return mcpserver.New(b, version).Run(ctx, &mcp.StdioTransport{})
	}

	if fs.NArg() > 0 {
		return fmt.Errorf("serve: unexpected argument %q", fs.Arg(0))
	}
	// Secrets first: the token variable may come from the secrets file too.
	token, err := httpToken(*secretsPath, *tokenEnv)
	if err != nil {
		return err
	}
	b, err := loadBridge(*cfgPath, "")
	if err != nil {
		return err
	}
	logger := log.New(stderr, "adminer-bridge: ", log.LstdFlags)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h := newHTTPHandler(mcpserver.New(b, version), token, logger)
	return serveHTTP(ctx, *httpAddr, h, logger)
}

func query(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config file")
	secretsPath := fs.String("secrets", "", "KEY=VALUE file (chmod 600) loaded into the environment before the config")
	env := fs.String("env", "", "environment name (required)")
	db := fs.String("db", "", "database (default: the environment's default database; must be an allowed database)")
	maxRows := fs.Int("max-rows", 0, "maximum rows (default 100, hard max 1000)")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	// Allow flags on either side of the SQL argument.
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if *env == "" || len(pos) != 1 {
		fmt.Fprint(stderr, usage)
		return errors.New("query needs --env and exactly one SQL argument")
	}
	b, err := loadBridge(*cfgPath, *secretsPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := b.Query(ctx, *env, *db, pos[0], *maxRows)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	return printTable(stdout, res)
}

func printTable(w io.Writer, res *bridge.Result) error {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	for i, c := range res.Columns {
		if i > 0 {
			fmt.Fprint(tw, "\t")
		}
		fmt.Fprint(tw, c)
	}
	fmt.Fprintln(tw)
	for _, row := range res.Rows {
		for i, cell := range row {
			if i > 0 {
				fmt.Fprint(tw, "\t")
			}
			if cell == nil {
				fmt.Fprint(tw, "NULL")
			} else {
				fmt.Fprint(tw, *cell)
			}
		}
		fmt.Fprintln(tw)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if res.Truncated {
		_, err := fmt.Fprintf(w, "(%d rows, truncated)\n", res.RowCount)
		return err
	}
	_, err := fmt.Fprintf(w, "(%d rows)\n", res.RowCount)
	return err
}
