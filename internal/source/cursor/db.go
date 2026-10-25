package cursor

import (
	"cmp"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	_ "modernc.org/sqlite"

	"github.com/emmaneugene/session-search/internal/source"
)

// The editor database is a key-value table, cursorDiskKV(key, value), with
// JSON stored as TEXT. Its schema is undocumented: every read here
// tolerates missing keys and falls back to the transcript alone.
//
//	composerData:<chat>            title, times, workspace, bubble order
//	composerConvHeaders:<chat>:<n> bubble order in schema 18 (chunks)
//	bubbleId:<chat>:<bubble>       one message, thinking block or tool call
func dbPath(env source.Env) string {
	return env.Path("Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
}

// openDB opens the database without writing anything next to it.
//
// mode=ro stops SQL writes but not file writes: on a WAL database whose
// -wal and -shm files are absent (Cursor isn't running), opening it creates
// them. So when either file is missing, the main file holds every committed
// page and immutable=1 reads it without creating files or taking locks.
// When both exist, Cursor is running (or crashed), the newest pages may be
// in the WAL, and mode=ro reads them through the existing -shm, as any other
// reader of the database does. If that open fails, immutable=1 is the
// fallback, without the newest WAL pages.
func openDB(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	base := "file:" + (&url.URL{Path: path}).EscapedPath() + "?mode=ro&_pragma=busy_timeout(1000)"
	dsns := []string{base + "&immutable=1"}
	if exists(path+"-wal") && exists(path+"-shm") {
		dsns = []string{base, base + "&immutable=1"}
	}
	var lastErr error
	for _, dsn := range dsns {
		db, err := sql.Open("sqlite", dsn)
		if err != nil {
			lastErr = err
			continue
		}
		var n int
		if err = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'cursorDiskKV'`).Scan(&n); err == nil && n == 1 {
			return db, nil
		}
		db.Close()
		lastErr = cmp.Or(err, errors.New("cursor: no cursorDiskKV table"))
	}
	return nil, lastErr
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// prefixRange is the key range [prefix, prefix with its last byte + 1),
// which uses the key index where LIKE would not.
func prefixRange(prefix string) (string, string) {
	return prefix, prefix[:len(prefix)-1] + string(prefix[len(prefix)-1]+1)
}

// markers returns each chat's lastUpdatedAt, as a change marker for
// fingerprints. It reads composerData rows only, never bubbles.
func markers(env source.Env) map[string]string {
	db, err := openDB(dbPath(env))
	if err != nil {
		return nil
	}
	defer db.Close()
	lo, hi := prefixRange("composerData:")
	rows, err := db.Query(`SELECT substr(key, 14), json_extract(CAST(value AS TEXT), '$.lastUpdatedAt')
		FROM cursorDiskKV WHERE key >= ? AND key < ? AND json_valid(CAST(value AS TEXT))`, lo, hi)
	if err != nil {
		return nil
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var id string
		var at sql.NullString
		if rows.Scan(&id, &at) == nil {
			m[id] = at.String
		}
	}
	return m
}

// chat is what the database holds for one chat.
type chat struct {
	composer gjson.Result
	bubbles  []bubble // in conversation order
}

type bubble struct {
	key     string // full row key, for Text
	user    bool
	text    string
	tool    string          // toolFormerData.name; empty when not a tool call
	result  string          // tool result text
	args    map[string]bool // string values in the tool's params and rawArgs
	created time.Time
	done    time.Time
}

// loadChat reads one chat's rows in three range queries and closes the
// database at once. ok is false when the chat isn't there.
func loadChat(env source.Env, id string) (c chat, ok bool) {
	db, err := openDB(dbPath(env))
	if err != nil {
		return c, false
	}
	defer db.Close()
	var raw sql.NullString
	if db.QueryRow(`SELECT CAST(value AS TEXT) FROM cursorDiskKV WHERE key = ?`, "composerData:"+id).Scan(&raw) != nil || !gjson.Valid(raw.String) {
		return c, false
	}
	c.composer = gjson.Parse(raw.String)

	var order []string
	addHeaders := func(h gjson.Result) {
		h.ForEach(func(_, x gjson.Result) bool {
			order = append(order, x.Get("bubbleId").String())
			return true
		})
	}
	// Schema 16 and 17 keep the order in composerData; schema 18 moved it
	// to composerConvHeaders chunks, numbered so key order is chunk order.
	addHeaders(c.composer.Get("fullConversationHeadersOnly"))
	if len(order) == 0 {
		eachRow(db, "composerConvHeaders:"+id+":", func(_ string, v string) { addHeaders(gjson.Parse(v)) })
	}

	rows := map[string]bubble{}
	prefix := "bubbleId:" + id + ":"
	eachRow(db, prefix, func(key, v string) {
		if gjson.Valid(v) {
			rows[strings.TrimPrefix(key, prefix)] = parseBubble(key, gjson.Parse(v))
		}
	})
	if len(order) == 0 {
		// No headers at all: fall back to creation time.
		for bid := range rows {
			order = append(order, bid)
		}
		slices.SortStableFunc(order, func(a, b string) int { return rows[a].created.Compare(rows[b].created) })
	}
	for _, bid := range order {
		if b, ok := rows[bid]; ok {
			c.bubbles = append(c.bubbles, b)
		}
	}
	return c, true
}

func eachRow(db *sql.DB, prefix string, fn func(key, value string)) {
	lo, hi := prefixRange(prefix)
	rows, err := db.Query(`SELECT key, CAST(value AS TEXT) FROM cursorDiskKV WHERE key >= ? AND key < ? ORDER BY key`, lo, hi)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v sql.NullString
		if rows.Scan(&k, &v) == nil && v.Valid {
			fn(k, v.String)
		}
	}
}

func parseBubble(key string, v gjson.Result) bubble {
	b := bubble{
		key:     key,
		user:    v.Get("type").Int() == 1,
		text:    v.Get("text").String(),
		created: source.ParseTime(v.Get("createdAt").String()),
		done:    source.MillisTime(v.Get("completedAtMs").Int()),
	}
	if b.created.IsZero() {
		b.created = source.MillisTime(v.Get("createdAt").Int())
	}
	if t := v.Get("toolFormerData"); t.Exists() {
		b.tool = t.Get("name").String()
		b.result = resultText(t.Get("result").String())
		b.args = leaves(t.Get("params").String())
		for k := range leaves(t.Get("rawArgs").String()) {
			b.args[k] = true
		}
	}
	return b
}

// bubbleResult reads one tool bubble's result text, for Text.
func bubbleResult(env source.Env, key string) (string, error) {
	db, err := openDB(dbPath(env))
	if err != nil {
		return "", err
	}
	defer db.Close()
	var v sql.NullString
	if err := db.QueryRow(`SELECT CAST(value AS TEXT) FROM cursorDiskKV WHERE key = ?`, key).Scan(&v); err != nil {
		return "", err
	}
	return parseBubble(key, gjson.Parse(v.String)).result, nil
}

// Result fields that hold no readable output.
var metaFields = map[string]bool{
	"totalLinesInFile": true, "beforeContentId": true, "afterContentId": true, "rejected": true,
	"notInterrupted": true, "exitCode": true, "success": true, "isBackground": true,
	"backgroundShellId": true, "isRunningInBackground": true, "poppedOutIntoBackground": true,
	"isFinal": true, "wasMerge": true,
}

// resultText turns toolFormerData.result, a JSON string whose shape depends
// on the tool, into the output a person would read: the shell output, the
// file contents, the fetched page, the MCP text or the error. Results that
// carry only metadata give "". Others are kept as compact JSON.
func resultText(r string) string {
	if r == "" || !gjson.Valid(r) {
		return r
	}
	v := gjson.Parse(r)
	if !v.IsObject() {
		return textOf(v)
	}
	for _, f := range []string{"output", "contents", "markdown", "content", "result", "error"} {
		if s := textOf(v.Get(f)); s != "" {
			return s
		}
	}
	useful := false
	v.ForEach(func(k, _ gjson.Result) bool {
		useful = !metaFields[k.String()]
		return !useful
	})
	if !useful {
		return ""
	}
	return gjson.Get(r, "@ugly").Raw
}

// textOf reads text from a string, a string holding JSON, an MCP content
// array, or an object with text, content, error or message.
func textOf(v gjson.Result) string {
	switch {
	case v.Type == gjson.String:
		s := v.String()
		if t := strings.TrimSpace(s); (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && gjson.Valid(t) {
			if inner := textOf(gjson.Parse(t)); inner != "" {
				return inner
			}
		}
		return s
	case v.IsArray():
		var parts []string
		v.ForEach(func(_, x gjson.Result) bool {
			if s := x.Get("text").String(); s != "" {
				parts = append(parts, s)
			}
			return true
		})
		return strings.Join(parts, "\n")
	case v.IsObject():
		for _, f := range []string{"text", "content", "error", "message"} {
			if s := textOf(v.Get(f)); s != "" {
				return s
			}
		}
	}
	return ""
}
