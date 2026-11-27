// Package mcpserver exposes the bridge as MCP tools. It is a thin layer:
// all validation and execution happen in the bridge.
package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/apikdech/adminer-bridge/bridge"
)

const readOnlyRules = "Read-only: send exactly one SELECT or EXPLAIN (no ANALYZE) per call. " +
	"Only built-in Postgres functions are allowed. " +
	"Literals must not contain ';' or newlines; use chr(59) for ';' and chr(10) for a newline. " +
	"Results are capped at 1000 rows. " +
	"If the read-only gate rejects the statement, read the rule name in the error and rewrite the statement."

type queryInput struct {
	Env      string `json:"env" jsonschema:"environment name (see list_environments)"`
	Database string `json:"database,omitempty" jsonschema:"Database to use. Optional; defaults to the environment's default database. Must be one of the environment's allowed databases (see list_environments)."`
	SQL      string `json:"sql" jsonschema:"a single SELECT or EXPLAIN statement"`
	MaxRows  int    `json:"max_rows,omitempty" jsonschema:"maximum rows to return (default 100, hard max 1000)"`
}

type listTablesInput struct {
	Env      string `json:"env" jsonschema:"environment name"`
	Database string `json:"database,omitempty" jsonschema:"Database to use. Optional; defaults to the environment's default database. Must be one of the environment's allowed databases (see list_environments)."`
	Schema   string `json:"schema,omitempty" jsonschema:"schema to list (default: the environment's schema)"`
}

type describeTableInput struct {
	Env      string `json:"env" jsonschema:"environment name"`
	Database string `json:"database,omitempty" jsonschema:"Database to use. Optional; defaults to the environment's default database. Must be one of the environment's allowed databases (see list_environments)."`
	Table    string `json:"table" jsonschema:"table name, optionally schema-qualified as schema.table"`
}

type listEnvironmentsInput struct{}

type listEnvironmentsOutput struct {
	Environments []bridge.EnvInfo `json:"environments"`
}

// New builds an MCP server exposing b's tools.
func New(b *bridge.Bridge, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "adminer-bridge", Version: version}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_environments",
		Description: "List the configured environments: default schema, default database, and the allowed databases with their connection facts (warnings). " + readOnlyRules,
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ listEnvironmentsInput) (*mcp.CallToolResult, listEnvironmentsOutput, error) {
		envs := b.ListEnvironments()
		if envs == nil {
			envs = []bridge.EnvInfo{}
		}
		return nil, listEnvironmentsOutput{Environments: envs}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "query",
		Description: "Run a read-only SQL statement against a Postgres environment. " + readOnlyRules,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in queryInput) (*mcp.CallToolResult, *bridge.Result, error) {
		res, err := b.Query(ctx, in.Env, in.Database, in.SQL, in.MaxRows)
		return nil, res, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_tables",
		Description: "List tables and views in a schema. " + readOnlyRules,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listTablesInput) (*mcp.CallToolResult, *bridge.Result, error) {
		res, err := b.ListTables(ctx, in.Env, in.Database, in.Schema)
		return nil, res, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "describe_table",
		Description: "Show columns, indexes and foreign keys of a table. " + readOnlyRules,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in describeTableInput) (*mcp.CallToolResult, *bridge.TableDescription, error) {
		res, err := b.DescribeTable(ctx, in.Env, in.Database, in.Table)
		return nil, res, err
	})

	return s
}
