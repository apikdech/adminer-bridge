package gate

import (
	"fmt"
	"strings"
	"time"
)

// Timeouts are the per-statement limits applied inside the wrapper
// transaction.
type Timeouts struct {
	Statement, Lock time.Duration
}

// WrappedStmtIndex is the index of the caller's statement among the
// statements of the batch Wrap returns.
const WrappedStmtIndex = 4

// Wrap puts a gated statement inside a read-only transaction with local
// timeouts and a final ROLLBACK. stmt must come from Validate.
func Wrap(stmt string, t Timeouts) string {
	parts := []string{
		"BEGIN TRANSACTION READ ONLY",
		fmt.Sprintf("SET LOCAL statement_timeout = '%dms'", t.Statement.Milliseconds()),
		fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", t.Lock.Milliseconds()),
		fmt.Sprintf("SET LOCAL idle_in_transaction_session_timeout = '%dms'", t.Statement.Milliseconds()),
		stmt,
		"ROLLBACK",
	}
	return strings.Join(parts, ";\n") + ";"
}
