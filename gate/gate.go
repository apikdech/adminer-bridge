// Package gate is the read-only SQL gate. Validate parses SQL with
// Postgres's own parser (pg_query), accepts only a single read-only
// SELECT or EXPLAIN, and returns the statement deparsed from the parse
// tree. Callers must send the returned text, never their input.
package gate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// Rule names the gate rule that rejected a statement.
type Rule string

const (
	RuleParse           Rule = "parse_error"
	RuleSingleStatement Rule = "single_statement"
	RuleStatementType   Rule = "statement_type" // top level not SELECT / EXPLAIN
	RuleExplainAnalyze  Rule = "explain_analyze"
	RuleNestedStatement Rule = "nested_statement" // any *Stmt other than SelectStmt below top level
	RuleSelectInto      Rule = "select_into"
	RuleLocking         Rule = "locking_clause"
	RuleFunction        Rule = "function_not_allowed"
	RuleUnsafeChars     Rule = "unsafe_characters" // ';' '\n' '\r' in deparsed text
	RuleDeparse         Rule = "deparse_error"
)

// RejectError reports why the gate rejected a statement.
type RejectError struct {
	Rule   Rule
	Detail string
}

func (e *RejectError) Error() string {
	return fmt.Sprintf("rejected by read-only gate [%s]: %s", e.Rule, e.Detail)
}

func reject(rule Rule, format string, args ...any) *RejectError {
	return &RejectError{Rule: rule, Detail: fmt.Sprintf(format, args...)}
}

// FuncSet holds built-in pg_catalog function names. A nil or empty set
// rejects every function call.
type FuncSet map[string]struct{}

// Validate checks sql against the read-only rules and returns the
// statement deparsed from its parse tree. Any rejection is a *RejectError.
func Validate(sql string, funcs FuncSet) (string, error) {
	tree, err := pg_query.Parse(sql)
	if err != nil {
		return "", reject(RuleParse, "%v", err)
	}
	if n := len(tree.GetStmts()); n != 1 {
		return "", reject(RuleSingleStatement, "expected exactly one statement, got %d", n)
	}

	jsonTree, err := pg_query.ParseToJSON(sql)
	if err != nil {
		return "", reject(RuleParse, "%v", err)
	}
	var doc struct {
		Stmts []struct {
			Stmt map[string]any `json:"stmt"`
		} `json:"stmts"`
	}
	if err := json.Unmarshal([]byte(jsonTree), &doc); err != nil {
		return "", reject(RuleParse, "decoding parse tree: %v", err)
	}
	if len(doc.Stmts) != 1 {
		return "", reject(RuleSingleStatement, "expected exactly one statement, got %d", len(doc.Stmts))
	}
	if rerr := checkTop(doc.Stmts[0].Stmt, funcs); rerr != nil {
		return "", rerr
	}

	out, err := pg_query.Deparse(tree)
	if err != nil {
		return "", reject(RuleDeparse, "%v", err)
	}
	if strings.ContainsAny(out, ";\n\r") {
		return "", reject(RuleUnsafeChars,
			"statement contains ';' or a line break (in a string, identifier or comment); build the character with chr(59) for ';' or chr(10) for a newline instead")
	}
	return out, nil
}

// checkTop checks the top-level statement node, which must be a single
// SelectStmt or ExplainStmt, and then walks everything below it.
func checkTop(stmt map[string]any, funcs FuncSet) *RejectError {
	if len(stmt) != 1 {
		return reject(RuleStatementType, "unexpected parse tree shape")
	}
	var kind string
	var body any
	for k, v := range stmt {
		kind, body = k, v
	}
	switch kind {
	case "SelectStmt":
	case "ExplainStmt":
		if hasAnalyzeOption(body) {
			return reject(RuleExplainAnalyze, "EXPLAIN ANALYZE executes the statement")
		}
	default:
		return reject(RuleStatementType, "only SELECT and EXPLAIN are allowed, got %s", kind)
	}
	return walk(body, funcs)
}

func hasAnalyzeOption(explain any) bool {
	m, _ := explain.(map[string]any)
	opts, _ := m["options"].([]any)
	for _, o := range opts {
		om, _ := o.(map[string]any)
		de, _ := om["DefElem"].(map[string]any)
		if name, _ := de["defname"].(string); strings.EqualFold(name, "analyze") {
			return true
		}
	}
	return false
}

// walk visits every node below the top-level statement. Map keys are
// visited in sorted order so the reported rule is deterministic.
func walk(node any, funcs FuncSet) *RejectError {
	switch n := node.(type) {
	case []any:
		for _, v := range n {
			if rerr := walk(v, funcs); rerr != nil {
				return rerr
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if rerr := checkKey(k, n[k], funcs); rerr != nil {
				return rerr
			}
			if rerr := walk(n[k], funcs); rerr != nil {
				return rerr
			}
		}
	}
	return nil
}

func checkKey(key string, value any, funcs FuncSet) *RejectError {
	switch {
	case key == "intoClause":
		return reject(RuleSelectInto, "SELECT ... INTO creates a table")
	case key == "lockingClause":
		return reject(RuleLocking, "FOR UPDATE / NO KEY UPDATE / SHARE / KEY SHARE take row locks")
	case key == "FuncCall":
		return checkFuncCall(value, funcs)
	case key != "SelectStmt" && strings.HasSuffix(key, "Stmt"):
		return reject(RuleNestedStatement, "nested %s is not allowed; only SELECT may appear inside a query", key)
	}
	return nil
}

// checkFuncCall decides whether a FuncCall node is allowed. No function
// allowlist exists yet, so every call is rejected.
func checkFuncCall(_ any, funcs FuncSet) *RejectError {
	if len(funcs) == 0 {
		return reject(RuleFunction, "function calls are rejected until the built-in function list is loaded")
	}
	return reject(RuleFunction, "function calls are not allowed")
}
