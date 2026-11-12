// Command adminer-bridge runs read-only SQL against Postgres through Adminer,
// either as an MCP stdio server (serve) or as a one-shot CLI (query).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"text/tabwriter"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"adminer-bridge/bridge"
	"adminer-bridge/config"
	"adminer-bridge/mcpserver"
)

var version = "dev"

const usage = `usage:
  adminer-bridge serve [--config path]
  adminer-bridge query [--config path] --env NAME [--max-rows N] [--json] "SQL"
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
	case "-h", "--help", "help":
		fmt.Fprint(stderr, usage)
		return flag.ErrHelp
	}
	fmt.Fprint(stderr, usage)
	return fmt.Errorf("unknown subcommand %q", args[0])
}

func loadBridge(path string) (*bridge.Bridge, error) {
	if path == "" {
		path = config.DefaultPath()
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return bridge.New(cfg)
}

func serve(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config file (default: $ADMINER_BRIDGE_CONFIG or ~/.config/adminer-bridge/config.yaml)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	b, err := loadBridge(*cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return mcpserver.New(b, version).Run(ctx, &mcp.StdioTransport{})
}

func query(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "config file")
	env := fs.String("env", "", "environment name (required)")
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
	b, err := loadBridge(*cfgPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := b.Query(ctx, *env, "", pos[0], *maxRows)
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
