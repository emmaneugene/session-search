// Package index keeps the SQLite full-text index of every session.
//
// The index is a cache. The transcript files are the source of truth, and
// the index can be deleted at any time. The full-text tables are
// "contentless": they hold the word index but not the text, so snippets are
// rebuilt by asking the source reader for an entry's text from its Loc.
package index

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"

	"github.com/emmaneugene/session-search/internal/model"
	"github.com/emmaneugene/session-search/internal/source"
)

// schemaVersion changes whenever the tables or what's indexed change. A
// different version makes Open rebuild the index from scratch.
const schemaVersion = "3"

// ToolResultHead and ToolResultTail are how many bytes of the start and end
// of each tool result go into the tool-result index.
const (
	ToolResultHead = 1024
	ToolResultTail = 1024
)

const schema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS sessions(
	rowid INTEGER PRIMARY KEY,
	agent TEXT NOT NULL,
	id TEXT NOT NULL,
	file TEXT NOT NULL UNIQUE,
	fingerprint TEXT NOT NULL,
	cwd TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	started INTEGER NOT NULL DEFAULT 0,
	updated INTEGER NOT NULL DEFAULT 0,
	parent TEXT NOT NULL DEFAULT '',
	lacks TEXT NOT NULL DEFAULT '',
	hidden INTEGER NOT NULL DEFAULT 0,
	entries INTEGER NOT NULL DEFAULT 0,
	parse_error TEXT NOT NULL DEFAULT '',
	failed_fingerprint TEXT NOT NULL DEFAULT '',
	failed_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS sessions_ref ON sessions(agent, id);
CREATE INDEX IF NOT EXISTS sessions_id ON sessions(id);
CREATE INDEX IF NOT EXISTS sessions_updated ON sessions(updated);
CREATE TABLE IF NOT EXISTS entries(
	rowid INTEGER PRIMARY KEY,
	session INTEGER NOT NULL,
	seq INTEGER NOT NULL,
	ts INTEGER NOT NULL DEFAULT 0,
	kind TEXT NOT NULL,
	tool TEXT NOT NULL DEFAULT '',
	abandoned INTEGER NOT NULL DEFAULT 0,
	loc_off INTEGER NOT NULL DEFAULT 0,
	loc_len INTEGER NOT NULL DEFAULT 0,
	loc_part INTEGER NOT NULL DEFAULT 0,
	loc_key TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS entries_session ON entries(session, seq);
CREATE TABLE IF NOT EXISTS dirs(path TEXT PRIMARY KEY, mtime INTEGER NOT NULL, items TEXT NOT NULL);
CREATE VIRTUAL TABLE IF NOT EXISTS text_fts USING fts5(text, content='', contentless_delete=1, tokenize='unicode61 remove_diacritics 2');
CREATE VIRTUAL TABLE IF NOT EXISTS tool_fts USING fts5(text, content='', contentless_delete=1, tokenize='unicode61 remove_diacritics 2');
`

// Index is an open index database.
type Index struct {
	db   *sql.DB
	path string
	lock *os.File
}

// DefaultPath is $SESSION_SEARCH_INDEX, or ~/.cache/session-search/index.db.
func DefaultPath(home string) string {
	if p := os.Getenv("SESSION_SEARCH_INDEX"); p != "" {
		return p
	}
	return filepath.Join(home, ".cache", "session-search", "index.db")
}

// Open opens or creates the index at path. rebuild deletes it first. A
// new or outdated schema also starts the index again. All of this happens
// under the update lock, so it can't delete files under a running sync.
func Open(path string, rebuild bool) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock, err := takeLock(path)
	if err != nil {
		return nil, err
	}
	defer releaseLock(lock)
	if rebuild {
		if err := Remove(path); err != nil {
			return nil, err
		}
	}
	ix, err := open(path)
	if err != nil {
		return nil, err
	}
	var v string
	_ = ix.db.QueryRow(`SELECT value FROM meta WHERE key='schema'`).Scan(&v)
	if v == schemaVersion {
		return ix, nil
	}
	ix.db.Close()
	if err := Remove(path); err != nil {
		return nil, err
	}
	if ix, err = open(path); err != nil {
		return nil, err
	}
	if _, err := ix.db.Exec(schema); err != nil {
		ix.db.Close()
		return nil, fmt.Errorf("create index schema: %w", err)
	}
	if _, err := ix.db.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES('schema', ?)`, schemaVersion); err != nil {
		ix.db.Close()
		return nil, err
	}
	return ix, nil
}

func open(path string) (*Index, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open index %s: %w", path, err)
	}
	_ = os.Chmod(path, 0o600)
	return &Index{db: db, path: path}, nil
}

// Remove deletes the index files.
func Remove(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Close closes the database and releases the update lock if held.
func (ix *Index) Close() error {
	ix.Unlock()
	return ix.db.Close()
}

// Path is the index file.
func (ix *Index) Path() string { return ix.path }

// Lock takes the update lock, waiting for another process that holds it,
// so two processes don't reparse the same files at once.
func (ix *Index) Lock() error {
	f, err := takeLock(ix.path)
	if err != nil {
		return err
	}
	ix.lock = f
	return nil
}

// Unlock releases the update lock.
func (ix *Index) Unlock() {
	releaseLock(ix.lock)
	ix.lock = nil
}

func takeLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func releaseLock(f *os.File) {
	if f != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}

// Known is what the index holds about one file.
type Known struct {
	Fingerprint string // of the last successful parse; "" if none
	// FailedFingerprint and FailedAt record the last failed parse, so a
	// broken file isn't parsed on every command.
	FailedFingerprint string
	FailedAt          time.Time
}

// Fingerprints returns what the index knows about every file of an agent.
func (ix *Index) Fingerprints(agent string) (map[string]Known, error) {
	rows, err := ix.db.Query(`SELECT file, fingerprint, failed_fingerprint, failed_at FROM sessions WHERE agent = ?`, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Known{}
	for rows.Next() {
		var f string
		var k Known
		var at int64
		if err := rows.Scan(&f, &k.Fingerprint, &k.FailedFingerprint, &at); err != nil {
			return nil, err
		}
		k.FailedAt = FromUnix(at)
		out[f] = k
	}
	return out, rows.Err()
}

// Tx is a write transaction.
type Tx struct{ tx *sql.Tx }

// Begin starts a write transaction.
func (ix *Index) Begin() (*Tx, error) {
	tx, err := ix.db.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx}, nil
}

func (t *Tx) Commit() error   { return t.tx.Commit() }
func (t *Tx) Rollback() error { return t.tx.Rollback() }

// Delete removes the session stored for file, with its entries.
func (t *Tx) Delete(file string) error {
	var rowid int64
	err := t.tx.QueryRow(`SELECT rowid FROM sessions WHERE file = ?`, file).Scan(&rowid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM text_fts WHERE rowid IN (SELECT rowid FROM entries WHERE session = ?)`,
		`DELETE FROM tool_fts WHERE rowid IN (SELECT rowid FROM entries WHERE session = ?)`,
		`DELETE FROM entries WHERE session = ?`,
		`DELETE FROM sessions WHERE rowid = ?`,
	} {
		if _, err := t.tx.Exec(q, rowid); err != nil {
			return err
		}
	}
	return nil
}

// Failed records a failed parse of file. The session's last good entries
// stay searchable. A file never parsed gets a placeholder row with no
// fingerprint, so it's tried again.
func (t *Tx) Failed(agent, file, fingerprint string, parseErr error, now time.Time) error {
	res, err := t.tx.Exec(`UPDATE sessions SET parse_error = ?, failed_fingerprint = ?, failed_at = ? WHERE file = ?`,
		parseErr.Error(), fingerprint, unix(now), file)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = t.tx.Exec(`INSERT INTO sessions(agent, id, file, fingerprint, parse_error, failed_fingerprint, failed_at) VALUES(?,?,?,?,?,?,?)`,
		agent, strings.TrimSuffix(filepath.Base(file), filepath.Ext(file)), file, "", parseErr.Error(), fingerprint, unix(now))
	return err
}

// Put replaces the stored session for s.File after a successful parse.
func (t *Tx) Put(s model.Session, fingerprint string, entries []model.Entry) error {
	if err := t.Delete(s.File); err != nil {
		return err
	}
	res, err := t.tx.Exec(`INSERT INTO sessions(agent, id, file, fingerprint, cwd, title, started, updated, parent, lacks, hidden, entries)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.Agent, s.ID, s.File, fingerprint, s.Cwd, s.Title, unix(s.Started), unix(s.Updated), s.Parent,
		strings.Join(s.Lacks, ","), boolInt(s.Hidden), len(entries))
	if err != nil {
		return err
	}
	sessionID, _ := res.LastInsertId()
	ins, err := t.tx.Prepare(`INSERT INTO entries(session, seq, ts, kind, tool, abandoned, loc_off, loc_len, loc_part, loc_key) VALUES(?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	insText, err := t.tx.Prepare(`INSERT INTO text_fts(rowid, text) VALUES(?, ?)`)
	if err != nil {
		return err
	}
	defer insText.Close()
	insTool, err := t.tx.Prepare(`INSERT INTO tool_fts(rowid, text) VALUES(?, ?)`)
	if err != nil {
		return err
	}
	defer insTool.Close()
	for _, e := range entries {
		text, target := indexedText(e)
		if target == "" {
			continue
		}
		res, err := ins.Exec(sessionID, e.Seq, unix(e.Time), string(e.Kind), e.Tool, boolInt(e.Abandoned),
			e.Loc.Offset, e.Loc.Length, e.Loc.Part, e.Loc.Key)
		if err != nil {
			return err
		}
		rowid, _ := res.LastInsertId()
		stmt := insText
		if target == "tool" {
			stmt = insTool
		}
		if _, err := stmt.Exec(rowid, text); err != nil {
			return err
		}
	}
	return nil
}

// indexedText decides where an entry is indexed: "text", "tool", or ""
// (not indexed), and what text goes in.
func indexedText(e model.Entry) (string, string) {
	if strings.TrimSpace(e.Text) == "" {
		return "", ""
	}
	switch e.Kind {
	case model.KindUser, model.KindAssistant, model.KindThinking, model.KindSummary:
		return e.Text, "text"
	case model.KindToolCall:
		return e.Tool + " " + e.Text, "text"
	case model.KindToolResult:
		return HeadTail(e.Text), "tool"
	}
	return "", "" // injected text repeats across sessions and isn't indexed
}

// HeadTail keeps the first and last kilobyte of a tool result.
func HeadTail(s string) string {
	if len(s) <= ToolResultHead+ToolResultTail {
		return s
	}
	head, tail := ToolResultHead, len(s)-ToolResultTail
	// Cut on character boundaries, not inside a multi-byte character.
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	return s[:head] + "\n…\n" + s[tail:]
}

// SetMeta stores a key in the meta table.
func (ix *Index) SetMeta(key, value string) error {
	_, err := ix.db.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES(?, ?)`, key, value)
	return err
}

// Meta reads a key from the meta table.
func (ix *Index) Meta(key string) string {
	var v string
	_ = ix.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	return v
}

// LoadDirs reads the saved folder listings.
func (ix *Index) LoadDirs() (map[string]source.CachedDir, error) {
	rows, err := ix.db.Query(`SELECT path, mtime, items FROM dirs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]source.CachedDir{}
	for rows.Next() {
		var p, items string
		var d source.CachedDir
		if err := rows.Scan(&p, &d.ModTime, &items); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(items), &d.Items) == nil {
			out[p] = d
		}
	}
	return out, rows.Err()
}

// SaveDirs stores changed folder listings and forgets old ones.
func (ix *Index) SaveDirs(save map[string]source.CachedDir, forget []string) error {
	if len(save) == 0 && len(forget) == 0 {
		return nil
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for p, d := range save {
		items, _ := json.Marshal(d.Items)
		if _, err := tx.Exec(`INSERT OR REPLACE INTO dirs(path, mtime, items) VALUES(?,?,?)`, p, d.ModTime, string(items)); err != nil {
			return err
		}
	}
	for _, p := range forget {
		if _, err := tx.Exec(`DELETE FROM dirs WHERE path = ?`, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DB exposes the database to the query code in this package's callers.
func (ix *Index) DB() *sql.DB { return ix.db }

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// FromUnix converts a stored millisecond time back.
func FromUnix(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
