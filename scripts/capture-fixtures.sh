#!/usr/bin/env bash
# Captures Adminer 5.4.1 HTML fixtures into adminer/testdata/.
# Requires ADMINER_LOCAL_PASSWORD in the environment. Never prints or stores it.
# The failed-login capture happens exactly once, last, in its own cookie jar.
set -euo pipefail

: "${ADMINER_LOCAL_PASSWORD:?set ADMINER_LOCAL_PASSWORD}"
BASE=${ADMINER_URL:-http://localhost:8080/}
OUT="$(cd "$(dirname "$0")/.." && pwd)/adminer/testdata"
mkdir -p "$OUT"
JAR=$(mktemp); JAR2=$(mktemp)
trap 'rm -f "$JAR" "$JAR2"' EXIT
SQLURL="${BASE}?pgsql=database&username=postgres&db=glchat&ns=public&sql="

# wrap <stmt>: same shape as gate.Wrap with 15s/2s timeouts.
wrap() {
  printf "BEGIN TRANSACTION READ ONLY;\nSET LOCAL statement_timeout = '15000ms';\nSET LOCAL lock_timeout = '2000ms';\nSET LOCAL idle_in_transaction_session_timeout = '15000ms';\n%s;\nROLLBACK;" "$1"
}

# 1. login page, no cookies
curl -sS -o "$OUT/login.html" "$BASE"

# 2. login (expect 302)
code=$(curl -sS -o /dev/null -w '%{http_code}' -c "$JAR" -b "$JAR" "$BASE" \
  --form-string 'auth[driver]=pgsql' --form-string 'auth[server]=database' \
  --form-string 'auth[username]=postgres' --form-string "auth[password]=$ADMINER_LOCAL_PASSWORD" \
  --form-string 'auth[db]=glchat')
[ "$code" = 302 ] || { echo "login returned $code, aborting (not retrying)" >&2; exit 1; }

# 3. SQL page + token
curl -sS -b "$JAR" -c "$JAR" -o "$OUT/sql_page.html" "$SQLURL"
TOKEN=$(grep -o "name=['\"]token['\"] value=['\"][^'\"]*" "$OUT/sql_page.html" | head -1 | sed "s/.*value=['\"]//")
[ -n "$TOKEN" ] || { echo "no token found" >&2; exit 1; }

# run <file> <stmt> [limit] [token]
run() {
  curl -sS -b "$JAR" -c "$JAR" -o "$OUT/$1" "$SQLURL" \
    --form-string "query=$(wrap "$2")" --form-string "limit=${3:-100}" \
    --form-string 'error_stops=1' --form-string "token=${4:-$TOKEN}"
}

run ok.html "SELECT 1 AS a, NULL AS b, 'NULL' AS c, '' AS d, '<b>&\"' AS e, 'é漢' AS f"
run multiline.html "SELECT E'line1\nline2' AS m, '  padded  ' AS p"
run empty.html "SELECT 1 AS a WHERE false"
run limit.html "SELECT g FROM generate_series(1,5) g" 3
run explain.html "EXPLAIN SELECT 1"
run error_stmt.html "SELECT * FROM no_such_table"
run error_readonly.html "CREATE TABLE zz_probe(i int)"
LONG="SELECT 1 AS c0"
for i in $(seq 1 200); do LONG="$LONG, 1 AS c$i"; done
run long_stmt.html "$LONG"
run bad_token.html "SELECT 1" 100 "1:2"

# 4. failed login: exactly once, last, separate cookie jar. Adminer answers 302; -L follows
# it (as GET) so the saved page carries the error.
curl -sSL -o "$OUT/login_failed.html" -c "$JAR2" -b "$JAR2" "$BASE" \
  --form-string 'auth[driver]=pgsql' --form-string 'auth[server]=database' \
  --form-string 'auth[username]=postgres' --form-string 'auth[password]=definitely-wrong-password' \
  --form-string 'auth[db]=glchat'
