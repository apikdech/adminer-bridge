# Adminer 5.4.1 rendering notes (from fixtures in this directory)

Captured by `scripts/capture-fixtures.sh` against the local Adminer. Task 5 copies
these into the comment at the top of `adminer/parse.go`.

## Structure of a SQL response

The batch runs as one POST. Adminer splits it into statements and echoes each one:

    <pre id='sql-N'><code class='jush-pgsql'>STATEMENT (HTML-escaped, no trailing ;)</code></pre>

followed by exactly one of:

- Statement without a result set (BEGIN, SET LOCAL, ROLLBACK):
  `<p class='message' title=''>Query executed OK, 0 rows affected. <span class='time'>(0.000 s)</span> <a href=...>Edit</a>`
- Statement with a result set:

      <div class='scrollable'>
      <table class='nowrap odds'>
      <thead><tr><th>a<th>b</thead>
      <tr><td class='number'>1<td><i>NULL</i></table>
      </div>
      <form action='' method='post'>
      <p class='sql-footer'>1 row <span class='time'>(0.000 s)</span> <a ...>Edit</a>, <a href='#explain-5'>Explain</a>...

  The HTML is not well-formed: `<th>`, `<td>`, `<tr>` are never closed. Use a real
  HTML parser (goquery handles it). The footer `<p class='sql-footer'>` holds the
  row count text ("1 row", "3 / 5 rows"), the time, and Edit/Explain/Export links.
- Failing statement: `<p class='error'>Error in query (7): ERROR:  <pg message></p>`

The wrapped batch gives sql-1..sql-4 (BEGIN + three SET LOCAL), sql-5 (the user
statement), sql-6 (ROLLBACK). The user statement is always `#sql-5`.

Statement echoes are HTML-escaped (`&#039;` for `'`, `&lt;`, `&amp;`).

## Zero rows (empty.html)

No table at all:

    <pre id='sql-5'>...SELECT 1 AS a WHERE false</code></pre>
    <p class='message'>No rows.
    <form action='' method='post'>
    <p class='sql-footer'> <span class='time'>(0.000 s)</span> ...

Note `<p class='message'>` here has no `title` attribute and its text is "No rows."
The column names are lost. The sql-footer has no row count text.

## Limit (limit.html, limit=3 on a 5-row query)

Only 3 rows are in the table. Truncation is shown only in the footer:

    <p class='sql-footer'>3 / 5 rows <span class='time'>(0.001 s)</span> ...

Without truncation the footer reads "1 row" / "N rows". "K / N rows" means K shown
of N total. Adminer reads the full result to count N.

## NULL, empty, padding, newlines (ok.html, multiline.html)

- SQL NULL: `<td><i>NULL</i>`
- The string 'NULL': `<td>NULL` (no `<i>`). This is the only way to tell them apart.
- Empty string: `<td>` followed directly by the next `<td>` (no content).
- Numbers get `class='number'`: `<td class='number'>1`.
- Values are HTML-escaped: `<td>&lt;b&gt;&amp;&quot;`. Unicode is raw UTF-8: `é漢`.
- Newlines inside a value are literal newlines inside the cell, not `<br>`:

      <tr><td>line1
      line2<td>  padded  </table>

  Padding spaces are preserved verbatim. Do not TrimSpace cell text.
  (The `nowrap` class and CSS affect display only.)

## EXPLAIN (explain.html) and div.explain (ok.html)

A user EXPLAIN is a normal result set (column `QUERY PLAN`, one row per plan line);
its footer has Export but no Explain link.

For ordinary SELECTs the footer has `<a href='#explain-5'>Explain</a>` and Adminer
adds, after the footer form and before the next `pre#sql-N`, a hidden block that
re-runs EXPLAIN and holds ANOTHER result table:

    <div id='explain-5' class='hidden explain'>
    <div class='scrollable'>
    <table class='nowrap odds'>
    <thead><tr><th>QUERY PLAN</thead>
    <tr><td>Result  (cost=0.00..0.01 rows=1 width=164)</table>
    </div>
    </div>

The parser MUST ignore tables inside `div.explain`, or it will see a bogus second
result for the statement.

## Long echoed statements (long_stmt.html)

Adminer SHORTENS long statements in `<pre id='sql-N'>`. A 1200+ char statement is
cut at about 1000 characters and the marker is an italic ellipsis element:

    ... 1 AS c99, 1 A<i>…</i></code></pre>

So never compare the echoed `<pre>` text to the sent statement for equality. The
full statement is still in the textarea (see below) and in the Edit link.

## Errors (error_stmt.html, error_readonly.html)

With `error_stops=1`, execution stops at the failing statement. The pre for the
failing statement is followed by:

    <pre id='sql-5'><code class='jush-pgsql'>CREATE TABLE zz_probe(i int)</code></pre>
    <p class='error'>Error in query (7): ERROR:  cannot execute CREATE TABLE in a read-only transaction
    <p class='error'>Error in query:  <a href='#sql-5'>5</a>

- The first `p.error` is the server message. For a missing relation it also has
  `<br>LINE 1: SELECT * FROM <b>no_such_table</b>` after the message (extra HTML).
- The second `p.error` is a summary: "Error in query:  <a href='#sql-N'>N</a>" with
  the index of the failing statement.
- No pre/messages for later statements (sql-6 ROLLBACK is not executed or shown).
- Read-only violation text: `cannot execute CREATE TABLE in a read-only transaction`
  (SQLSTATE 25006, shown as "(7)" which is Adminer's own error code).
- After an error, the page re-shows the whole batch in
  `<textarea name='query' ...>` inside `form#form`, with `'` escaped as `&#039;`.

## CSRF failure (bad_token.html)

Posting `token=1:2` returns the full SQL page with no `pre#sql-N` blocks and:

    <div class='error'>Invalid CSRF token. Send the form again. If you did not send this request from Adminer then close this page.</div>

Note this is `div.error`, not `p.error`. Detect on the text "Invalid CSRF token".

## Token and session

- The CSRF token is `<input type='hidden' name='token' value='933214:708207'>` (single
  quotes). The same token value also appears in small forms after results (export), but
  `form#form` holds exactly one `input[name=token]`; scope to it.
- Script tags carry a per-response `nonce`. It does not matter for parsing.
- No session cookie value or password appears in any fixture.

## Login page (login.html)

Title `Login - Adminer`. Fields: `auth[driver]`, `auth[server]`, `auth[username]`,
`auth[password]`, `auth[db]`.

## Login failure (login_failed.html: NOT captured)

The fixture is missing. A failed login POST answers 302 and the first capture
did not follow the redirect (empty body). A second failed attempt was not made
because Adminer throttles IPs. The script now uses `curl -L` for that step.
Message text found in the Adminer source in the container (not seen in a captured
page): `Invalid credentials.` After too many failures Adminer shows
`Too many unsuccessful logins, try again in N minute(s).`
