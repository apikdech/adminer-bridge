package gate

import (
	"errors"
	"testing"
)

var testFuncs = FuncSet{
	"count": {}, "lower": {}, "row_number": {}, "now": {}, "pg_sleep": {},
	"query_to_xml": {}, "set_config": {}, "pg_terminate_backend": {},
	"lo_import": {}, "pg_advisory_lock": {}, "pg_ls_dir": {},
	"pg_read_file": {}, "table_to_xml": {}, "nextval": {}, "pg_stat_reset": {},
}

func TestFuncsAllowed(t *testing.T) {
	cases := []string{
		"select count(*) from users",
		"select lower(name) from users",
		"select row_number() over (order by id) from users",
		"select pg_catalog.lower('A')",
		"select now()",
		"select * from users where id in (select 1)",
	}
	for _, sql := range cases {
		t.Run(sql, func(t *testing.T) {
			if _, err := Validate(sql, testFuncs); err != nil {
				t.Fatalf("Validate(%q) = %v, want nil", sql, err)
			}
		})
	}
}

func TestFuncsRejected(t *testing.T) {
	cases := []struct {
		sql  string
		rule Rule
	}{
		{"select query_to_xml('delete from users', true, false, '')", RuleFunction},
		{"select dblink('x','delete from users')", RuleFunction},
		{`select "PG_SLEEP"(1)`, RuleFunction},
		{"select public.lower('a')", RuleFunction},
		{"select x.pg_catalog.lower('a')", RuleFunction},
		{"select my_func()", RuleFunction},
		{"select * from pg_sleep(1)", RuleFunction},
		{"select * from (select pg_sleep(1)) s", RuleFunction},
		{"with c as (select pg_sleep(1)) select * from c", RuleFunction},
		{"select 1 from users where pg_sleep(1) is null", RuleFunction},
		{"select id from users order by pg_sleep(1)", RuleFunction},
		{"select case when true then pg_sleep(1) end", RuleFunction},
		{"explain select pg_sleep(1)", RuleFunction},

		{"select $1", RuleUnsafeChars},
		{"select * from users where id = $1", RuleUnsafeChars},
	}
	for _, name := range []string{
		"pg_sleep", "query_to_xml", "set_config", "pg_terminate_backend",
		"lo_import", "pg_advisory_lock", "pg_ls_dir", "pg_read_file",
		"table_to_xml", "nextval", "pg_stat_reset",
	} {
		args := "(1)"
		cases = append(cases,
			struct {
				sql  string
				rule Rule
			}{"select " + name + args, RuleFunction},
			struct {
				sql  string
				rule Rule
			}{"select pg_catalog." + name + args, RuleFunction},
			struct {
				sql  string
				rule Rule
			}{"select " + upper(name) + args, RuleFunction},
		)
	}
	for _, c := range cases {
		t.Run(c.sql, func(t *testing.T) {
			out, err := Validate(c.sql, testFuncs)
			var re *RejectError
			if !errors.As(err, &re) {
				t.Fatalf("Validate(%q) = (%q, %v), want *RejectError rule %s", c.sql, out, err, c.rule)
			}
			if re.Rule != c.rule {
				t.Fatalf("Validate(%q) rule = %s (%s), want %s", c.sql, re.Rule, re.Detail, c.rule)
			}
		})
	}
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

func TestNilFuncSetRejectsAllCalls(t *testing.T) {
	_, err := Validate("select count(*) from users", nil)
	var re *RejectError
	if !errors.As(err, &re) || re.Rule != RuleFunction {
		t.Fatalf("got %v, want function_not_allowed", err)
	}
}

func TestBlocked(t *testing.T) {
	cases := map[string]bool{
		"pg_sleep":                         true,
		"pg_terminate_backend":             true,
		"set_config":                       true,
		"nextval":                          true,
		"pg_advisory_xact_lock":            true,
		"pg_try_advisory_lock":             true,
		"database_to_xml":                  true,
		"cursor_to_xmlschema":              true,
		"pg_create_restore_point":          true,
		"pg_drop_replication_slot":         true,
		"lo_unlink":                        true,
		"dblink_exec":                      true,
		"pg_copy_logical_replication_slot": true,
		"PG_SLEEP":                         true,
		"lower":                            false,
		"pg_get_constraintdef":             false,
		"count":                            false,
	}
	for name, want := range cases {
		if got := Blocked(name); got != want {
			t.Errorf("Blocked(%q) = %v, want %v", name, got, want)
		}
	}
}
