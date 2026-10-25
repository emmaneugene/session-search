package engine

import (
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/emmaneugene/session-search/internal/index"
	"github.com/emmaneugene/session-search/internal/model"
	"github.com/emmaneugene/session-search/internal/source"
)

// SearchOptions are the search command's settings.
type SearchOptions struct {
	Query       string
	Filter      index.Filter
	Kinds       []model.Kind // empty: user, assistant, summary
	ToolResults bool         // search the tool-result table instead
	Limit       int          // sessions
	PerSession  int          // matches shown per session
}

// Match is one matching entry inside a session result.
type Match struct {
	Seq       int        `json:"seq"`
	Time      time.Time  `json:"time,omitzero"`
	Kind      model.Kind `json:"kind"`
	Tool      string     `json:"tool,omitempty"`
	Abandoned bool       `json:"abandoned,omitempty"`
	Snippet   string     `json:"snippet"`
}

// Result is one session in the search results.
type Result struct {
	Ref     string   `json:"ref"`
	Agent   string   `json:"agent"`
	ID      string   `json:"id"`
	Title   string   `json:"title,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	File    string   `json:"file"`
	Started string   `json:"started,omitempty"`
	Updated string   `json:"updated,omitempty"`
	Parent  string   `json:"parent,omitempty"`
	Lacks   []string `json:"lacks,omitempty"`
	// Matched is "message" when one message holds every query word,
	// "session" when the words are spread over several messages, and
	// "some" when the session holds only some of them.
	Matched string  `json:"matched"`
	Matches []Match `json:"matches"`
}

// DefaultKinds are the entry kinds searched when the caller doesn't say.
var DefaultKinds = []model.Kind{model.KindUser, model.KindAssistant, model.KindSummary}

// Terms splits a query into words and "quoted phrases".
func Terms(q string) []string {
	var terms []string
	for len(q) > 0 {
		q = strings.TrimLeftFunc(q, unicode.IsSpace)
		if q == "" {
			break
		}
		if q[0] == '"' {
			end := strings.IndexByte(q[1:], '"')
			if end < 0 {
				end = len(q) - 1
			}
			if t := strings.TrimSpace(q[1 : end+1]); t != "" {
				terms = append(terms, t)
			}
			q = q[min(end+2, len(q)):]
			continue
		}
		end := strings.IndexFunc(q, unicode.IsSpace)
		if end < 0 {
			end = len(q)
		}
		if t := q[:end]; strings.IndexFunc(t, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0 {
			terms = append(terms, t)
		}
		q = q[end:]
	}
	return terms
}

// ftsQuery quotes each term as an FTS5 phrase and joins them with op.
func ftsQuery(terms []string, op string) string {
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	if len(quoted) == 1 {
		return quoted[0]
	}
	return strings.Join(quoted, " "+op+" ")
}

// Search runs a search. The index must already be in sync.
//
// Results come in three tiers. First, sessions with a message that holds
// every query word, ranked by their best messages. Second, sessions that hold
// every word across several messages. Third, sessions that hold some of the
// words, ranked by how many. A session's score in the second and third
// tiers adds up its best score for each word.
func Search(ix *index.Index, env source.Env, srcs map[string]source.Source, o SearchOptions) ([]Result, error) {
	terms := Terms(o.Query)
	if len(terms) == 0 {
		return nil, nil
	}
	kinds := o.Kinds
	if len(kinds) == 0 && !o.ToolResults {
		kinds = DefaultKinds
	}
	limit := max(o.Limit, 1)
	per := max(o.PerSession, 1)

	results := map[int64]*resultState{}
	var order []*resultState
	// Three entries per session feed the session score (see addScore).
	perScore := max(per, 3)
	hits, err := ix.Search(ftsQuery(terms, "AND"), o.ToolResults, kinds, o.Filter, perScore, limit*perScore)
	if err != nil {
		return nil, err
	}
	for _, h := range hits {
		r := results[h.SessionID]
		if r == nil {
			r = newResult(h.Session, "message")
			results[h.SessionID] = r
			order = append(order, r)
		}
		r.addScore(h.Score)
		if len(r.Matches) < per {
			r.addMatch(h)
		}
	}
	sortResults(order)

	if len(order) < limit && len(terms) > 1 {
		more, err := spreadMatches(ix, terms, kinds, o, results)
		if err != nil {
			return nil, err
		}
		for _, r := range more {
			if len(order) >= limit {
				break
			}
			order = append(order, r)
		}
	}
	if len(order) > limit {
		order = order[:limit]
	}

	out := make([]Result, len(order))
	for i, r := range order {
		src := srcs[r.Agent]
		for j := range r.Matches {
			if src == nil {
				continue
			}
			text, err := src.Text(env, r.File, r.locs[j])
			if err != nil {
				r.Matches[j].Snippet = "(snippet unavailable: " + err.Error() + ")"
				continue
			}
			r.Matches[j].Snippet = Snippet(text, terms, 240)
		}
		out[i] = r.Result
	}
	return out, nil
}

// spreadMatches finds sessions, other than those in skip, that hold the
// query words across messages: every word first, then some words.
func spreadMatches(ix *index.Index, terms []string, kinds []model.Kind, o SearchOptions, skip map[int64]*resultState) ([]*resultState, error) {
	type agg struct {
		session index.Session
		words   int
		score   float64
	}
	// Candidates are the sessions of the best entries matching any word;
	// scoring every session that holds a common word would be slow.
	hits, err := ix.Search(ftsQuery(terms, "OR"), o.ToolResults, kinds, o.Filter, max(o.PerSession, 1), 300*max(o.PerSession, 1))
	if err != nil {
		return nil, err
	}
	var candidates []int64
	best := map[int64][]index.Hit{} // each candidate's best entries, for snippets
	for _, h := range hits {
		if skip[h.SessionID] != nil {
			continue
		}
		if best[h.SessionID] == nil {
			candidates = append(candidates, h.SessionID)
		}
		if len(best[h.SessionID]) < max(o.PerSession, 1) {
			best[h.SessionID] = append(best[h.SessionID], h)
		}
	}
	sessions := map[int64]*agg{}
	for _, t := range terms {
		scores, err := ix.TermSessions(ftsQuery([]string{t}, ""), o.ToolResults, kinds, candidates)
		if err != nil {
			return nil, err
		}
		for id, score := range scores {
			if skip[id] != nil {
				continue
			}
			a := sessions[id]
			if a == nil {
				a = &agg{}
				sessions[id] = a
			}
			a.words++
			a.score += score
		}
	}
	ids := make([]int64, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := sessions[ids[i]], sessions[ids[j]]
		if a.words != b.words {
			return a.words > b.words
		}
		return a.score < b.score
	})
	if len(ids) > o.Limit {
		ids = ids[:max(o.Limit, 1)]
	}
	out := make([]*resultState, 0, len(ids))
	for _, id := range ids {
		hs := best[id]
		matched := "some"
		if sessions[id].words == len(terms) {
			matched = "session"
		}
		r := newResult(hs[0].Session, matched)
		r.score = sessions[id].score
		for _, h := range hs {
			r.addMatch(h)
		}
		out = append(out, r)
	}
	return out, nil
}

func sortResults(rs []*resultState) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].score != rs[j].score {
			return rs[i].score < rs[j].score
		}
		return rs[i].Updated > rs[j].Updated
	})
}

// resultState carries ranking state next to the public Result.
type resultState struct {
	Result
	row   int64
	locs  []model.Loc
	score float64
	terms int
}

func (r *resultState) addMatch(h index.Hit) {
	r.Matches = append(r.Matches, Match{Seq: h.Seq, Time: h.Time, Kind: h.Kind, Tool: h.Tool, Abandoned: h.Abandoned})
	r.locs = append(r.locs, h.Loc)
}

func newResult(s index.Session, matched string) *resultState {
	return &resultState{row: s.Row, Result: Result{
		Ref: s.Ref(), Agent: s.Agent, ID: s.ID, Title: s.Title, Cwd: s.Cwd, File: s.File,
		Started: fmtTime(s.Started), Updated: fmtTime(s.Updated), Parent: s.Parent, Lacks: s.Lacks,
		Matched: matched,
	}}
}

// addScore folds an entry's bm25 score into the session score. The best
// entry counts fully and the next two count less, so a session that
// discusses the topic several times beats one passing mention.
func (r *resultState) addScore(s float64) {
	weights := []float64{1, 0.5, 0.25}
	if r.terms < len(weights) {
		r.score += s * weights[r.terms]
	}
	r.terms++
}

// Snippet returns up to width characters of text around the first query
// term it finds, on one line.
func Snippet(text string, terms []string, width int) string {
	flat := strings.Join(strings.Fields(text), " ")
	lower := strings.ToLower(flat)
	at := -1
	for _, t := range terms {
		if i := strings.Index(lower, strings.ToLower(t)); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	runes := []rune(flat)
	if len(runes) <= width {
		return flat
	}
	start := 0
	if at > 0 {
		start = max(0, len([]rune(flat[:at]))-width/3)
	}
	end := min(len(runes), start+width)
	s := string(runes[start:end])
	if start > 0 {
		s = "…" + s
	}
	if end < len(runes) {
		s += "…"
	}
	return s
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
