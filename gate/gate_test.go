package gate

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestValidateAllows(t *testing.T) {
	cases := []string{
		"select * from users",
		"select u.id from users u join orgs o on o.id = u.org_id where u.id = 1",
		"with x as (select 1 as a) select a from x",
		"select * from (select 1) s",
		"explain select * from users",
		"explain (format json) select 1",
		"select id from users order by id limit 5 offset 2",
	}
	for _, sql := range cases {
		t.Run(sql, func(t *testing.T) {
			if _, err := Validate(sql, nil); err != nil {
				t.Fatalf("Validate(%q) = %v, want nil", sql, err)
			}
		})
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		sql  string
		rule Rule
	}{
		{"select 1; select 2", RuleSingleStatement},
		{"select 1; drop table users", RuleSingleStatement},
		{"select 1 /* ; */ ; delete from users", RuleSingleStatement},
		{"select 1 -- x\n; delete from users", RuleSingleStatement},
		{"commit; delete from users", RuleSingleStatement},

		{"delete from users", RuleStatementType},
		{"update users set a=1", RuleStatementType},
		{"insert into users values (1)", RuleStatementType},
		{"merge into t using s on true when matched then delete", RuleStatementType},
		{"drop table users", RuleStatementType},
		{"truncate users", RuleStatementType},
		{"copy users to stdout", RuleStatementType},
		{"set role postgres", RuleStatementType},
		{"reset all", RuleStatementType},
		{"do $$ begin delete from users; end $$", RuleStatementType},
		{"call p()", RuleStatementType},
		{"prepare p as select 1", RuleStatementType},
		{"execute p", RuleStatementType},
		{"listen x", RuleStatementType},
		{"notify x", RuleStatementType},
		{"vacuum users", RuleStatementType},
		{"analyze users", RuleStatementType},
		{"begin", RuleStatementType},
		{"commit", RuleStatementType},
		{"rollback", RuleStatementType},
		{"lock table users", RuleStatementType},
		{"create table t (i int)", RuleStatementType},
		{"grant select on users to public", RuleStatementType},
		{"refresh materialized view mv", RuleStatementType},
		{"discard all", RuleStatementType},
		{"checkpoint", RuleStatementType},
		{"load 'x'", RuleStatementType},
		{"show all", RuleStatementType},

		{"with d as (delete from users returning *) select * from d", RuleNestedStatement},
		{"with u as (update users set a=1 returning *) select 1", RuleNestedStatement},
		{"with i as (insert into t values (1) returning *) select 1", RuleNestedStatement},

		{"select * into newtab from users", RuleSelectInto},

		{"select * from users for update", RuleLocking},
		{"select * from users for share", RuleLocking},
		{"select * from users for no key update", RuleLocking},
		{"select * from users for key share", RuleLocking},
		{"select * from users for update skip locked", RuleLocking},

		{"explain analyze delete from users", RuleExplainAnalyze},
		{"explain analyze select 1", RuleExplainAnalyze},
		{"explain (analyze) select 1", RuleExplainAnalyze},
		{"explain (analyze false) select 1", RuleExplainAnalyze},
		{"explain (ANALYZE, buffers) select 1", RuleExplainAnalyze},

		{"explain delete from users", RuleNestedStatement},

		{"select 'a;b' as x", RuleUnsafeChars},
		{"select $$a\nb$$", RuleUnsafeChars},
		{"select E'a\\nb'", RuleUnsafeChars},
		{`select 1 as "we;ird"`, RuleUnsafeChars},

		{"select count(*) from users", RuleFunction},
		{"select pg_catalog.pg_sleep(1)", RuleFunction},
	}
	for _, c := range cases {
		t.Run(c.sql, func(t *testing.T) {
			out, err := Validate(c.sql, nil)
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

func TestValidateRejectsUnparseable(t *testing.T) {
	for _, sql := range []string{"selec 1", "", "   "} {
		t.Run(sql, func(t *testing.T) {
			_, err := Validate(sql, nil)
			var re *RejectError
			if !errors.As(err, &re) {
				t.Fatalf("Validate(%q) err = %v, want *RejectError", sql, err)
			}
		})
	}
}

func TestValidateUnicodeIdentifiersDoNotPanic(t *testing.T) {
	for _, sql := range []string{
		`select U&"\0064elete"`,
		`select U&"\0064elete" from users`,
		`select * from U&"d\0061ta"`,
		`select U&'\003B'`,
	} {
		t.Run(sql, func(t *testing.T) {
			_, _ = Validate(sql, nil)
		})
	}
}

func TestValidateReturnsDeparsed(t *testing.T) {
	got, err := Validate("SELECT   *   FROM users WHERE id=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "SELECT * FROM users WHERE id = 1"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRejectErrorMessage(t *testing.T) {
	_, err := Validate("delete from users", nil)
	if err == nil {
		t.Fatal("want error")
	}
	if p := "rejected by read-only gate [statement_type]:"; !strings.HasPrefix(err.Error(), p) {
		t.Fatalf("Error() = %q, want prefix %q", err.Error(), p)
	}
}

func TestWrap(t *testing.T) {
	got := Wrap("SELECT 1", Timeouts{15 * time.Second, 2 * time.Second})
	want := "BEGIN TRANSACTION READ ONLY;\nSET LOCAL statement_timeout = '15000ms';\nSET LOCAL lock_timeout = '2000ms';\nSET LOCAL idle_in_transaction_session_timeout = '15000ms';\nSELECT 1;\nROLLBACK;"
	if got != want {
		t.Fatalf("Wrap =\n%s\nwant\n%s", got, want)
	}
	if parts := strings.Split(got, ";\n"); parts[WrappedStmtIndex] != "SELECT 1" {
		t.Fatalf("statement index %d = %q", WrappedStmtIndex, parts[WrappedStmtIndex])
	}
}
