// Package adminer talks to Adminer's web UI and parses its result pages.
//
// Adminer 5.4.1 rendering findings (from adminer/testdata, see NOTES.md):
//
//   - Each statement is echoed as <pre id='sql-N'><code>…</code></pre>, HTML-escaped,
//     then exactly one of: <p class='message'> (no result set, or "No rows." for an
//     empty result set: no table at all), <div class='scrollable'><table> (result
//     set), or <p class='error'> (statement failure).
//   - Long echoed SQL is cut at ~1000 chars and ends with <i>…</i> inside the pre:
//     Truncated=true, and the marker is excluded from SQL.
//   - A failed statement yields a p.error with the server message, then a summary
//     p.error "Error in query:  <a href='#sql-N'>N</a>" which is ignored. Later
//     statements are not shown (error_stops).
//   - Page-level failures (invalid CSRF token, session expired, invalid credentials,
//     throttling) are div.error, not p.error.
//   - Ordinary SELECTs are followed by a hidden div.explain holding another result
//     table; it must be ignored.
//   - Table markup leaves th/td/tr unclosed; goquery (x/net/html) copes.
//   - Cell values: SQL NULL is <td><i>NULL</i>; the string 'NULL' is plain text;
//     the empty string is an empty td. Values are HTML-escaped, unicode is raw,
//     newlines are literal (not <br>), padding is preserved: never trim cells.
//   - Each form carries its own CSRF token; take the one in form#form (or the
//     login form's).
package adminer

import (
	"bytes"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

// BlockKind says what Adminer rendered after an echoed statement.
type BlockKind int

const (
	KindMessage BlockKind = iota
	KindTable
	KindError
)

// Block is the result of one statement in the batch.
type Block struct {
	SQL       string // echoed statement text, entity-decoded, trimmed
	Truncated bool   // Adminer shortened the echoed SQL
	Kind      BlockKind
	Message   string      // KindMessage / KindError text
	Columns   []string    // KindTable
	Rows      [][]*string // KindTable; nil = SQL NULL
}

// PageKind says which Adminer page was returned.
type PageKind int

const (
	PageSQL PageKind = iota
	PageLogin
)

// ParsePage parses an Adminer response page. pageErr is a page-level error not
// attached to a statement (login failure, invalid CSRF token, throttling).
func ParsePage(page []byte) (kind PageKind, blocks []Block, token string, pageErr string, err error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(page))
	if err != nil {
		return 0, nil, "", "", err
	}

	if doc.Find(`input[name="auth[password]"]`).Length() > 0 {
		kind = PageLogin
	}
	// Adminer 5.4.1: the login form itself has no token; the page's language form does.
	// Adminer 6: the login form has one too, and the login POST must echo it.
	token = attr(doc.Find(`form#form input[name=token]`).First(), "value")
	if token == "" {
		token = attr(doc.Find(`input[name=token]`).First(), "value")
	}

	var cur *Block
	flush := func() {
		if cur != nil {
			blocks = append(blocks, *cur)
			cur = nil
		}
	}
	doc.Find("#content").First().Children().Each(func(_ int, s *goquery.Selection) {
		switch {
		case s.Is("pre[id^=sql-]"):
			flush()
			cur = &Block{}
			code := s.Find("code").First()
			if code.Length() == 0 {
				code = s
			}
			code = code.Clone()
			if m := code.Find("i"); m.Length() > 0 {
				cur.Truncated = true
				m.Remove()
			}
			cur.SQL = strings.TrimSpace(code.Text())
		case cur == nil:
			if pageErr == "" && s.Is(".error") {
				pageErr = strings.TrimSpace(text(s))
			}
		case cur.Message != "" || cur.Kind != KindMessage:
			// kind already decided for this statement
		case s.Is("p.error"):
			if s.Find("a[href^='#sql-']").Length() > 0 {
				return // "Error in query: N" summary
			}
			cur.Kind = KindError
			cur.Message = strings.TrimSpace(text(s))
		case s.Is("p.message"):
			cur.Message = strings.TrimSpace(text(s))
		case s.Is("div.scrollable"):
			if t := s.Find("table").First(); t.Length() > 0 {
				cur.Kind = KindTable
				cur.Columns, cur.Rows = parseTable(t)
			}
		}
	})
	flush()
	return kind, blocks, token, pageErr, nil
}

func parseTable(t *goquery.Selection) ([]string, [][]*string) {
	cols := []string{}
	t.Find("thead th").Each(func(_ int, th *goquery.Selection) {
		cols = append(cols, th.Text())
	})
	rows := [][]*string{}
	t.Find("tbody > tr").Each(func(_ int, tr *goquery.Selection) {
		row := []*string{}
		tr.ChildrenFiltered("td").Each(func(_ int, td *goquery.Selection) {
			row = append(row, cell(td))
		})
		rows = append(rows, row)
	})
	return cols, rows
}

// cell returns nil for SQL NULL (<td><i>NULL</i>), else the exact text.
func cell(td *goquery.Selection) *string {
	if n := td.Get(0); n.FirstChild != nil && n.FirstChild == n.LastChild &&
		n.FirstChild.Type == html.ElementNode && n.FirstChild.Data == "i" &&
		goquery.NewDocumentFromNode(n.FirstChild).Text() == "NULL" {
		return nil
	}
	s := text(td)
	return &s
}

// text returns the selection's text with <br> as "\n".
func text(s *goquery.Selection) string {
	c := s.Clone()
	c.Find("br").ReplaceWithHtml("\n")
	return c.Text()
}

func attr(s *goquery.Selection, name string) string {
	v, _ := s.Attr(name)
	return v
}
