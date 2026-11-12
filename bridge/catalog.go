package bridge

import (
	"context"
	"fmt"
	"strings"
)

// TableDescription is the structure of one table.
type TableDescription struct {
	Schema      string  `json:"schema"`
	Table       string  `json:"table"`
	Columns     *Result `json:"columns"`      // column_name, data_type, is_nullable, column_default
	Indexes     *Result `json:"indexes"`      // indexname, indexdef
	ForeignKeys *Result `json:"foreign_keys"` // conname, definition
}

// quoteLiteral renders s as a SQL string literal. The result always goes
// through the gate, so anything odd is rejected there.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// envSchema returns the configured default schema of env.
func (b *Bridge) envSchema(env string) (string, error) {
	st, ok := b.envs[env]
	if !ok {
		return "", fmt.Errorf("unknown environment %q; valid environments: %s", env, strings.Join(b.cfg.Names(), ", "))
	}
	return st.env.Schema, nil
}

// ListTables lists tables and views in schema ("" means the environment's schema).
func (b *Bridge) ListTables(ctx context.Context, env, schema string) (*Result, error) {
	def, err := b.envSchema(env)
	if err != nil {
		return nil, err
	}
	if schema == "" {
		schema = def
	}
	sql := "SELECT table_name, table_type FROM information_schema.tables" +
		" WHERE table_schema = " + quoteLiteral(schema) +
		" AND table_type IN ('BASE TABLE', 'VIEW') ORDER BY table_name"
	return b.query(ctx, env, sql, 0)
}

// DescribeTable returns columns, indexes and foreign keys of table, which may
// be "schema.table" (split on the first dot) or a bare name in the env schema.
func (b *Bridge) DescribeTable(ctx context.Context, env, table string) (*TableDescription, error) {
	schema, err := b.envSchema(env)
	if err != nil {
		return nil, err
	}
	if s, t, ok := strings.Cut(table, "."); ok {
		schema, table = s, t
	}
	sch, tbl := quoteLiteral(schema), quoteLiteral(table)

	cols, err := b.query(ctx, env,
		"SELECT column_name, data_type, is_nullable, column_default FROM information_schema.columns"+
			" WHERE table_schema = "+sch+" AND table_name = "+tbl+" ORDER BY ordinal_position", 0)
	if err != nil {
		return nil, err
	}
	if len(cols.Rows) == 0 {
		return nil, fmt.Errorf("table %s.%s not found", schema, table)
	}
	idx, err := b.query(ctx, env,
		"SELECT indexname, indexdef FROM pg_catalog.pg_indexes"+
			" WHERE schemaname = "+sch+" AND tablename = "+tbl+" ORDER BY indexname", 0)
	if err != nil {
		return nil, err
	}
	fks, err := b.query(ctx, env,
		"SELECT c.conname, pg_catalog.pg_get_constraintdef(c.oid) AS definition FROM pg_catalog.pg_constraint c"+
			" JOIN pg_catalog.pg_class r ON r.oid = c.conrelid"+
			" JOIN pg_catalog.pg_namespace n ON n.oid = r.relnamespace"+
			" WHERE c.contype = 'f' AND n.nspname = "+sch+" AND r.relname = "+tbl+" ORDER BY c.conname", 0)
	if err != nil {
		return nil, err
	}
	return &TableDescription{Schema: schema, Table: table, Columns: cols, Indexes: idx, ForeignKeys: fks}, nil
}
