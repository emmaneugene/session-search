package index

import (
	"database/sql"
	"strings"
	"time"

	"github.com/emmaneugene/session-search/internal/model"
)

// Filter narrows sessions in search and list.
type Filter struct {
	Agents        []string
	Cwd           string // substring of the session cwd
	Since, Until  time.Time
	IncludeHidden bool
	ExcludeIDs    []string
}

// Session is a stored session plus index bookkeeping.
type Session struct {
	model.Session
	Entries    int    `json:"indexed_entries"`
	ParseError string `json:"parse_error,omitempty"`
	Row        int64  `json:"-"` // index row
}

// Hit is one matching entry.
type Hit struct {
	Session   Session
	SessionID int64 // index row of the session
	Seq       int
	Time      time.Time
	Kind      model.Kind
	Tool      string
	Abandoned bool
	Loc       model.Loc
	Score     float64 // bm25: lower is better
}

const sessionCols = `s.rowid, s.agent, s.id, s.file, s.cwd, s.title, s.started, s.updated, s.parent, s.lacks, s.hidden, s.entries, s.parse_error`

func scanSession(sc interface{ Scan(...any) error }, extra ...any) (Session, error) {
	var (
		s                Session
		rowid            int64
		started, updated int64
		lacks            string
		hidden           int
	)
	dest := append([]any{&rowid, &s.Agent, &s.ID, &s.File, &s.Cwd, &s.Title, &started, &updated, &s.Parent, &lacks, &hidden, &s.Entries, &s.ParseError}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return s, err
	}
	s.Started, s.Updated, s.Hidden, s.Row = FromUnix(started), FromUnix(updated), hidden == 1, rowid
	if lacks != "" {
		s.Lacks = strings.Split(lacks, ",")
	}
	return s, nil
}

func (f Filter) where() (string, []any) {
	// A row with no fingerprint is a placeholder for a file that never parsed.
	conds := []string{"s.fingerprint != ''"}
	var args []any
	if len(f.Agents) > 0 {
		conds = append(conds, "s.agent IN ("+placeholders(len(f.Agents))+")")
		for _, a := range f.Agents {
			args = append(args, a)
		}
	}
	if f.Cwd != "" {
		conds = append(conds, "instr(lower(s.cwd), lower(?)) > 0")
		args = append(args, f.Cwd)
	}
	if !f.Since.IsZero() {
		conds = append(conds, "s.updated >= ?")
		args = append(args, f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		conds = append(conds, "s.started <= ?")
		args = append(args, f.Until.UnixMilli())
	}
	if !f.IncludeHidden {
		conds = append(conds, "s.hidden = 0")
	}
	if len(f.ExcludeIDs) > 0 {
		conds = append(conds, "s.id NOT IN ("+placeholders(len(f.ExcludeIDs))+")")
		for _, id := range f.ExcludeIDs {
			args = append(args, id)
		}
	}
	return strings.Join(conds, " AND "), args
}

// Search returns matching entries, best first: at most perSession from
// each session and at most limit in all, so one session with many matches
// can't crowd out the others. match is an FTS5 query. toolResults chooses
// the tool-result table instead of the text table. kinds limits the entry
// kinds (empty means all in the table).
func (ix *Index) Search(match string, toolResults bool, kinds []model.Kind, f Filter, perSession, limit int) ([]Hit, error) {
	table := ftsTable(toolResults)
	where, args := f.where()
	inner := `SELECT e.rowid AS entry, e.session AS sess, bm25(` + table + `) AS score
		FROM ` + table + ` JOIN entries e ON e.rowid = ` + table + `.rowid JOIN sessions s ON s.rowid = e.session
		WHERE ` + table + ` MATCH ? AND ` + where
	qargs := append([]any{match}, args...)
	inner, qargs = withKinds(inner, qargs, kinds)
	// bm25() only works in the query that runs the MATCH; the LIMIT keeps
	// SQLite from flattening that query into the window query around it.
	q := `WITH m AS (` + inner + ` LIMIT -1),
		r AS (SELECT entry, sess, score, row_number() OVER (PARTITION BY sess ORDER BY score) AS rn FROM m)
		SELECT ` + sessionCols + `, e.seq, e.ts, e.kind, e.tool, e.abandoned, e.loc_off, e.loc_len, e.loc_part, e.loc_key, r.score
		FROM r JOIN entries e ON e.rowid = r.entry JOIN sessions s ON s.rowid = r.sess
		WHERE r.rn <= ? ORDER BY r.score LIMIT ?`
	qargs = append(qargs, max(perSession, 1), limit)
	rows, err := ix.db.Query(q, qargs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var (
			h         Hit
			ts        int64
			kind      string
			abandoned int
		)
		s, err := scanSession(rows, &h.Seq, &ts, &kind, &h.Tool, &abandoned, &h.Loc.Offset, &h.Loc.Length, &h.Loc.Part, &h.Loc.Key, &h.Score)
		if err != nil {
			return nil, err
		}
		h.Session, h.SessionID, h.Time, h.Kind, h.Abandoned = s, s.Row, FromUnix(ts), model.Kind(kind), abandoned == 1
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// TermSessions returns, for each of the given sessions with an entry
// matching match, the best (lowest) bm25 score of those entries.
func (ix *Index) TermSessions(match string, toolResults bool, kinds []model.Kind, sessions []int64) (map[int64]float64, error) {
	if len(sessions) == 0 {
		return map[int64]float64{}, nil
	}
	table := ftsTable(toolResults)
	where := "e.session IN (" + placeholders(len(sessions)) + ")"
	var args []any
	for _, id := range sessions {
		args = append(args, id)
	}
	// bm25() can't sit inside an aggregate, so score in a subquery. The
	// LIMIT keeps SQLite from flattening the subquery into the aggregate.
	q := `SELECT e.session AS session, bm25(` + table + `) AS score FROM ` + table + ` JOIN entries e ON e.rowid = ` + table + `.rowid
		WHERE ` + table + ` MATCH ? AND ` + where
	qargs := append([]any{match}, args...)
	q, qargs = withKinds(q, qargs, kinds)
	rows, err := ix.db.Query(`SELECT session, min(score) FROM (`+q+` LIMIT -1) GROUP BY session`, qargs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]float64{}
	for rows.Next() {
		var id int64
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			return nil, err
		}
		out[id] = score
	}
	return out, rows.Err()
}

func ftsTable(toolResults bool) string {
	if toolResults {
		return "tool_fts"
	}
	return "text_fts"
}

func withKinds(q string, args []any, kinds []model.Kind) (string, []any) {
	if len(kinds) == 0 {
		return q, args
	}
	q += " AND e.kind IN (" + placeholders(len(kinds)) + ")"
	for _, k := range kinds {
		args = append(args, string(k))
	}
	return q, args
}

// List returns sessions, most recently updated first.
func (ix *Index) List(f Filter, limit int) ([]Session, error) {
	where, args := f.where()
	rows, err := ix.db.Query(`SELECT `+sessionCols+` FROM sessions s WHERE `+where+` ORDER BY s.updated DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSessions(rows)
}

// Resolve finds sessions by "agent:id", "agent:id prefix", "id", "id
// prefix" or file path. It returns at most 10 candidates.
func (ix *Index) Resolve(ref string) ([]Session, error) {
	agent, id, hasAgent := strings.Cut(ref, ":")
	if !hasAgent || strings.Contains(agent, "/") {
		agent, id = "", ref
	}
	var rows *sql.Rows
	var err error
	switch {
	case strings.ContainsRune(ref, '/'):
		rows, err = ix.db.Query(`SELECT `+sessionCols+` FROM sessions s WHERE s.file = ?`, ref)
	case agent != "":
		rows, err = ix.db.Query(`SELECT `+sessionCols+` FROM sessions s WHERE s.agent = ? AND (s.id = ? OR s.id GLOB ?) ORDER BY s.id = ? DESC LIMIT 10`, agent, id, globPrefix(id), id)
	default:
		rows, err = ix.db.Query(`SELECT `+sessionCols+` FROM sessions s WHERE s.id = ? OR s.id GLOB ? ORDER BY s.id = ? DESC LIMIT 10`, id, globPrefix(id), id)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSessions(rows)
}

// AgentCounts returns indexed session counts and failures per agent.
func (ix *Index) AgentCounts() (map[string][2]int, error) {
	rows, err := ix.db.Query(`SELECT agent, count(*), sum(parse_error != '') FROM sessions GROUP BY agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]int{}
	for rows.Next() {
		var a string
		var n, failed int
		if err := rows.Scan(&a, &n, &failed); err != nil {
			return nil, err
		}
		out[a] = [2]int{n, failed}
	}
	return out, rows.Err()
}

func collectSessions(rows *sql.Rows) ([]Session, error) {
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func globPrefix(id string) string {
	r := strings.NewReplacer("*", "[*]", "?", "[?]", "[", "[[]")
	return r.Replace(id) + "*"
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
