package adminer

import (
	"os"
	"strings"
	"testing"
)

func load(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parse(t *testing.T, name string) (PageKind, []Block, string, string) {
	t.Helper()
	k, blocks, token, pageErr, err := ParsePage(load(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return k, blocks, token, pageErr
}

func sp(s string) *string { return &s }

func eqCell(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func TestOK(t *testing.T) {
	k, blocks, _, pageErr := parse(t, "ok.html")
	if k != PageSQL || pageErr != "" {
		t.Fatalf("kind=%v pageErr=%q", k, pageErr)
	}
	if len(blocks) != 6 {
		t.Fatalf("want 6 blocks, got %d", len(blocks))
	}
	for i, b := range blocks {
		want := KindMessage
		if i == 4 {
			want = KindTable
		}
		if b.Kind != want {
			t.Errorf("block %d kind=%v want %v", i, b.Kind, want)
		}
		for _, c := range b.Columns {
			if c == "QUERY PLAN" {
				t.Errorf("block %d leaked div.explain table", i)
			}
		}
	}
	b := blocks[4]
	if !strings.HasPrefix(b.SQL, "SELECT 1 AS a") {
		t.Errorf("SQL=%q", b.SQL)
	}
	if b.Truncated {
		t.Error("unexpected Truncated")
	}
	if strings.Join(b.Columns, ",") != "a,b,c,d,e,f" {
		t.Errorf("columns=%v", b.Columns)
	}
	want := []*string{sp("1"), nil, sp("NULL"), sp(""), sp("<b>&\""), sp("é漢")}
	if len(b.Rows) != 1 || len(b.Rows[0]) != len(want) {
		t.Fatalf("rows=%v", b.Rows)
	}
	for i := range want {
		if !eqCell(b.Rows[0][i], want[i]) {
			t.Errorf("cell %d = %v want %v", i, b.Rows[0][i], want[i])
		}
	}
	if blocks[0].SQL != "BEGIN TRANSACTION READ ONLY" {
		t.Errorf("block0 SQL=%q", blocks[0].SQL)
	}
	if blocks[1].SQL != "SET LOCAL statement_timeout = '15000ms'" {
		t.Errorf("block1 SQL=%q", blocks[1].SQL)
	}
}

func TestMultiline(t *testing.T) {
	_, blocks, _, _ := parse(t, "multiline.html")
	b := blocks[4]
	if b.Kind != KindTable || len(b.Rows) != 1 {
		t.Fatalf("block=%+v", b)
	}
	if b.Rows[0][0] == nil || *b.Rows[0][0] != "line1\nline2" {
		t.Errorf("cell0=%v", b.Rows[0][0])
	}
	if b.Rows[0][1] == nil || *b.Rows[0][1] != "  padded  " {
		t.Errorf("cell1=%v", b.Rows[0][1])
	}
}

func TestEmpty(t *testing.T) {
	_, blocks, _, _ := parse(t, "empty.html")
	if len(blocks) != 6 {
		t.Fatalf("blocks=%d", len(blocks))
	}
	b := blocks[4]
	if b.Kind != KindMessage || len(b.Rows) != 0 || !strings.Contains(b.Message, "No rows") {
		t.Errorf("block=%+v", b)
	}
}

func TestExplain(t *testing.T) {
	_, blocks, _, _ := parse(t, "explain.html")
	b := blocks[4]
	if b.Kind != KindTable || len(b.Columns) != 1 || b.Columns[0] != "QUERY PLAN" {
		t.Errorf("block=%+v", b)
	}
}

func TestErrors(t *testing.T) {
	_, blocks, _, pageErr := parse(t, "error_stmt.html")
	if pageErr != "" {
		t.Errorf("pageErr=%q", pageErr)
	}
	if len(blocks) != 5 {
		t.Fatalf("blocks=%d", len(blocks))
	}
	if blocks[4].Kind != KindError || !strings.Contains(blocks[4].Message, "no_such_table") {
		t.Errorf("block=%+v", blocks[4])
	}
	if strings.Contains(blocks[4].Message, "Error in query:") {
		t.Errorf("summary leaked: %q", blocks[4].Message)
	}
	_, blocks, _, _ = parse(t, "error_readonly.html")
	if len(blocks) != 5 || blocks[4].Kind != KindError || !strings.Contains(blocks[4].Message, "read-only transaction") {
		t.Errorf("blocks=%+v", blocks)
	}
}

func TestLongStmt(t *testing.T) {
	_, blocks, _, _ := parse(t, "long_stmt.html")
	b := blocks[4]
	if !b.Truncated {
		t.Error("want Truncated")
	}
	if strings.Contains(b.SQL, "…") {
		t.Error("marker leaked into SQL")
	}
	for _, o := range blocks[:4] {
		if o.Truncated {
			t.Error("short stmt marked truncated")
		}
	}
}

func TestLogin(t *testing.T) {
	k, blocks, token, pageErr := parse(t, "login.html")
	if k != PageLogin || len(blocks) != 0 || token == "" || pageErr != "" {
		t.Errorf("k=%v blocks=%d token=%q err=%q", k, len(blocks), token, pageErr)
	}
	k, _, _, pageErr = parse(t, "login_failed.html")
	if k != PageLogin || pageErr == "" {
		t.Errorf("k=%v err=%q", k, pageErr)
	}
}

func TestBadToken(t *testing.T) {
	k, blocks, _, pageErr := parse(t, "bad_token.html")
	if k != PageSQL || len(blocks) != 0 || !strings.Contains(pageErr, "Invalid CSRF token") {
		t.Errorf("k=%v blocks=%d err=%q", k, len(blocks), pageErr)
	}
}

func TestSQLPage(t *testing.T) {
	k, blocks, token, pageErr := parse(t, "sql_page.html")
	if k != PageSQL || len(blocks) != 0 || token == "" || pageErr != "" {
		t.Errorf("k=%v blocks=%d token=%q err=%q", k, len(blocks), token, pageErr)
	}
}

func TestLimitFixtureStillTable(t *testing.T) {
	_, blocks, _, _ := parse(t, "limit.html")
	if blocks[4].Kind != KindTable || len(blocks[4].Rows) != 3 {
		t.Errorf("block=%+v", blocks[4])
	}
}
