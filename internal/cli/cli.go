// Package cli implements the session-search commands.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/emmaneugene/session-search/internal/engine"
	"github.com/emmaneugene/session-search/internal/index"
	"github.com/emmaneugene/session-search/internal/model"
	"github.com/emmaneugene/session-search/internal/source"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitNoMatch = 1 // search found nothing, or a session reference matched nothing or several
	ExitUsage   = 2
	ExitError   = 3
)

const usage = `session-search searches and reads past AI coding agent sessions from Pi,
Cursor, Claude Code and Codex. It reads each agent's default folders and keeps
a full-text index in ~/.cache/session-search, which it updates before every
command. It never writes to any agent's files.

Usage:
  session-search search <query> [flags]   find sessions; JSON output
  session-search read <ref> [flags]       show part of one session; Markdown or JSON
  session-search list [flags]             recent sessions; JSON output
  session-search resolve <ref>            find one session by id, id prefix or file; JSON
  session-search sources                  what each agent has and the index state; JSON
  session-search index [--rebuild]        update or rebuild the index; JSON

A <ref> is agent:id (pi:0199aa12), an id or unique id prefix, or a file path.
Agents: pi, cursor, claude, codex.

Run "session-search <command> --help" for the command's flags.

Environment:
  SESSION_SEARCH_HOME    read agent folders under this folder instead of $HOME
  SESSION_SEARCH_INDEX   index file (default ~/.cache/session-search/index.db)

JSON output is compact; add --pretty to any command to indent it.

Exit codes: 0 found, 1 no match, 2 usage error, 3 other error.
`

const searchHelp = `Usage: session-search search <query> [flags]

Finds sessions that contain the words of the query, best first. Sessions where
one message holds every word come first ("matched": "message"). If there are
fewer than --limit of those, sessions holding every word across several
messages follow ("session"), then sessions holding some of the words
("some"). Put a phrase in quotes to match it exactly. Output is JSON: {"meta": {...}, "results": [...]}. Each
result has the session ref and up to --per-session matching entries with
their seq, which "read <ref> --at <seq>" shows in context.

By default the search covers user and assistant messages and summaries. The
current session ($PI_SESSION_ID or $CURSOR_CONVERSATION_ID) is left out.

Flags:
  --agent LIST           only these agents, comma separated (pi,cursor,claude,codex)
  --cwd TEXT             only sessions whose working folder contains TEXT
  --since DATE           only sessions updated since DATE (2026-10-23, RFC 3339, or 7d, 12h)
  --until DATE           only sessions started before DATE
  --role ROLE            user, assistant or any (default any)
  --include-tool-calls   also search tool call arguments and commands
  --include-thinking     also search model thinking
  --in tool-results      search tool output instead (first and last 1 KB of each)
  --limit N              sessions to return (default 10)
  --per-session N        matching entries per session (default 3)
  --include-current      include the current session
  --include-hidden       include hidden sessions (Codex review threads, sessions with no user message)
`

const readHelp = `Usage: session-search read <ref> [flags]
       session-search read --current [flags]

Shows part of one session as Markdown (or JSON with --format json). Without a
position flag it starts at the first entry. Tool results, thinking, injected
context (AGENTS.md, environment blocks) and abandoned branches are hidden
unless you ask for them. Long entries are clipped.

Flags:
  --current                the session this command runs in ($PI_SESSION_FILE or $CURSOR_CONVERSATION_ID)
  --at SEQ                 centre on entry SEQ (from a search result)
  --around TIME            centre on the entry nearest TIME (RFC 3339)
  --offset N               start at the Nth shown entry (1-based)
  --context N              entries on each side of --at or --around (default 6)
  --max N                  most entries to show (default 30)
  --max-bytes N            most bytes of output (default 60000)
  --include-tool-results   show tool output
  --include-thinking       show model thinking
  --include-injected       show injected context
  --include-abandoned      show entries on abandoned branches
  --format md|json         output format (default md)
`

const listHelp = `Usage: session-search list [flags]

Lists sessions, most recently updated first, as JSON.

Flags:
  --agent LIST       only these agents, comma separated
  --cwd TEXT         only sessions whose working folder contains TEXT
  --since DATE       only sessions updated since DATE
  --until DATE       only sessions started before DATE
  --live             only sessions updated in the last hour
  --limit N          sessions to return (default 20)
  --include-hidden   include hidden sessions
`

const resolveHelp = `Usage: session-search resolve <ref>

Finds one session by agent:id, id, unique id prefix or file path, and prints
it as JSON. Exits 1 when nothing or more than one session matches; the error
lists the candidates.
`

const sourcesHelp = `Usage: session-search sources

Prints, as JSON, how many session files each agent has, how many are indexed
and how many failed to parse, and the index file, size and last update.
`

const indexHelp = `Usage: session-search index [--rebuild]

Brings the index up to date and prints what changed as JSON. Every other
command does this first, so you only need it to build the index ahead of
time. --rebuild deletes the index and builds it again.
`

// Main runs the command line and returns the exit code.
func Main(argv []string, stdout, stderr io.Writer, srcs []source.Source) int {
	if len(argv) == 0 || argv[0] == "help" || argv[0] == "--help" || argv[0] == "-h" {
		fmt.Fprint(stdout, usage)
		if len(argv) == 0 {
			return ExitUsage
		}
		return ExitOK
	}
	c := &cmd{stdout: stdout, stderr: stderr, srcs: srcs, now: time.Now()}
	run, help, spec := c.command(argv[0])
	if run == nil {
		fmt.Fprintf(stderr, "session-search: unknown command %q\n\n%s", argv[0], usage)
		return ExitUsage
	}
	a, err := parseArgs(argv[1:], spec)
	if err != nil {
		fmt.Fprintf(stderr, "session-search %s: %v\n\n%s", argv[0], err, help)
		return ExitUsage
	}
	if a.has("help") {
		fmt.Fprint(stdout, help)
		return ExitOK
	}
	pretty = a.has("pretty")
	code, err := run(a)
	if err != nil {
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintf(stderr, "session-search %s: %v\n\n%s", argv[0], err, help)
			return ExitUsage
		}
		fmt.Fprintf(stderr, "session-search %s: %v\n", argv[0], err)
		if code == ExitOK {
			code = ExitError
		}
	}
	return code
}

type usageError struct{ error }

func usagef(format string, a ...any) error { return usageError{fmt.Errorf(format, a...)} }

type cmd struct {
	stdout, stderr io.Writer
	srcs           []source.Source
	now            time.Time
	env            source.Env
	ix             *index.Index
	sync           []engine.SyncStats
}

func (c *cmd) command(name string) (func(*args) (int, error), string, flagSpec) {
	switch name {
	case "search":
		return c.search, searchHelp, flagSpec{
			values: []string{"agent", "cwd", "since", "until", "role", "in", "limit", "per-session"},
			bools:  []string{"include-tool-calls", "include-thinking", "include-current", "include-hidden"},
		}
	case "read":
		return c.read, readHelp, flagSpec{
			values: []string{"at", "around", "offset", "context", "max", "max-bytes", "format"},
			bools:  []string{"current", "include-tool-results", "include-thinking", "include-injected", "include-abandoned"},
		}
	case "list":
		return c.list, listHelp, flagSpec{
			values: []string{"agent", "cwd", "since", "until", "limit"},
			bools:  []string{"live", "include-hidden"},
		}
	case "resolve":
		return c.resolve, resolveHelp, flagSpec{}
	case "sources":
		return c.sources, sourcesHelp, flagSpec{}
	case "index":
		return c.index, indexHelp, flagSpec{bools: []string{"rebuild"}}
	}
	return nil, "", flagSpec{}
}

// open finds the home folder, opens the index and syncs it.
func (c *cmd) open(rebuild bool) error {
	home := os.Getenv("SESSION_SEARCH_HOME")
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return err
		}
	}
	c.env = source.Env{Home: home}
	path := index.DefaultPath(home)
	t0 := time.Now()
	ix, err := index.Open(path, rebuild)
	if err != nil {
		return err
	}
	c.ix = ix
	saved, err := ix.LoadDirs()
	if err != nil {
		return err
	}
	c.env.Dirs = source.NewDirCache(saved)
	t1 := time.Now()
	c.sync, err = engine.Sync(ix, c.env, c.srcs, c.stderr)
	if err == nil {
		err = ix.SaveDirs(c.env.Dirs.Changed())
	}
	if os.Getenv("SESSION_SEARCH_DEBUG") != "" {
		fmt.Fprintf(c.stderr, "session-search: open %v, sync %v\n", t1.Sub(t0), time.Since(t1))
	}
	return err
}

func (c *cmd) close() {
	if c.ix != nil {
		c.ix.Close()
	}
}

func (c *cmd) sourceMap() map[string]source.Source {
	m := map[string]source.Source{}
	for _, s := range c.srcs {
		m[s.Agent()] = s
	}
	return m
}

func (c *cmd) agents() []string {
	var out []string
	for _, s := range c.srcs {
		out = append(out, s.Agent())
	}
	return out
}

func (c *cmd) filter(a *args) (index.Filter, error) {
	f := index.Filter{Cwd: a.str("cwd"), IncludeHidden: a.has("include-hidden")}
	if v := a.str("agent"); v != "" {
		f.Agents = splitList(v)
		for _, ag := range f.Agents {
			if !slices.Contains(c.agents(), ag) {
				return f, usagef("unknown agent %q (agents: %s)", ag, strings.Join(c.agents(), ", "))
			}
		}
	}
	var err error
	if f.Since, err = a.date("since", c.now); err != nil {
		return f, usagef("%v", err)
	}
	if f.Until, err = a.date("until", c.now); err != nil {
		return f, usagef("%v", err)
	}
	return f, nil
}

// currentIDs are the ids of the session this process runs in.
func currentIDs() []string {
	var ids []string
	for _, name := range []string{"PI_SESSION_ID", "CURSOR_CONVERSATION_ID"} {
		if v := os.Getenv(name); v != "" {
			ids = append(ids, v)
		}
	}
	return ids
}

func (c *cmd) search(a *args) (int, error) {
	if len(a.pos) == 0 {
		return ExitUsage, usagef("give a query")
	}
	f, err := c.filter(a)
	if err != nil {
		return ExitUsage, err
	}
	if !a.has("include-current") {
		f.ExcludeIDs = currentIDs()
	}
	o := engine.SearchOptions{Query: strings.Join(a.pos, " "), Filter: f}
	switch a.str("in") {
	case "", "text":
	case "tool-results":
		o.ToolResults = true
	default:
		return ExitUsage, usagef("--in takes text or tool-results")
	}
	if !o.ToolResults {
		switch a.str("role") {
		case "", "any":
			o.Kinds = slices.Clone(engine.DefaultKinds)
		case "user":
			o.Kinds = []model.Kind{model.KindUser}
		case "assistant":
			o.Kinds = []model.Kind{model.KindAssistant, model.KindSummary}
		default:
			return ExitUsage, usagef("--role takes user, assistant or any")
		}
		if a.has("include-tool-calls") {
			o.Kinds = append(o.Kinds, model.KindToolCall)
		}
		if a.has("include-thinking") {
			o.Kinds = append(o.Kinds, model.KindThinking)
		}
	}
	if o.Limit, err = a.int("limit", 10); err != nil {
		return ExitUsage, usagef("%v", err)
	}
	if o.PerSession, err = a.int("per-session", 3); err != nil {
		return ExitUsage, usagef("%v", err)
	}
	if len(engine.Terms(o.Query)) == 0 {
		return ExitUsage, usagef("the query has no words to search for")
	}

	start := time.Now()
	if err := c.open(false); err != nil {
		return ExitError, err
	}
	defer c.close()
	results, err := engine.Search(c.ix, c.env, c.sourceMap(), o)
	if err != nil {
		return ExitError, err
	}
	in := "text"
	if o.ToolResults {
		in = "tool-results"
	}
	out := map[string]any{
		"meta": map[string]any{
			"query":   o.Query,
			"terms":   engine.Terms(o.Query),
			"in":      in,
			"results": len(results),
			"limit":   o.Limit,
			"took_ms": time.Since(start).Milliseconds(),
			"index":   c.indexMeta(),
		},
		"results": nonNil(results),
	}
	if err := writeJSON(c.stdout, out); err != nil {
		return ExitError, err
	}
	if len(results) == 0 {
		return ExitNoMatch, nil
	}
	return ExitOK, nil
}

func (c *cmd) indexMeta() map[string]any {
	reindexed := 0
	for _, s := range c.sync {
		reindexed += s.Changed + s.Removed
	}
	return map[string]any{"reindexed": reindexed, "last_sync": c.ix.Meta("last_sync")}
}

func (c *cmd) read(a *args) (int, error) {
	if len(a.pos) > 1 || (len(a.pos) == 1) == a.has("current") {
		return ExitUsage, usagef("give one session ref, or --current")
	}
	o := engine.ReadOptions{
		At:          -1,
		ToolResults: a.has("include-tool-results"),
		Thinking:    a.has("include-thinking"),
		Injected:    a.has("include-injected"),
		Abandoned:   a.has("include-abandoned"),
	}
	var err error
	if v := a.str("at"); v != "" {
		if o.At, err = a.int("at", -1); err != nil {
			return ExitUsage, usagef("%v", err)
		}
	}
	if v := a.str("around"); v != "" {
		if o.Around, err = time.Parse(time.RFC3339, v); err != nil {
			return ExitUsage, usagef("--around takes an RFC 3339 time, got %q", v)
		}
	}
	if o.Offset, err = a.int("offset", 0); err != nil {
		return ExitUsage, usagef("%v", err)
	}
	positions := 0
	for _, set := range []bool{o.At >= 0, !o.Around.IsZero(), o.Offset > 0} {
		if set {
			positions++
		}
	}
	if positions > 1 {
		return ExitUsage, usagef("use only one of --at, --around and --offset")
	}
	if o.Context, err = a.int("context", 6); err != nil {
		return ExitUsage, usagef("%v", err)
	}
	if o.Max, err = a.int("max", 30); err != nil {
		return ExitUsage, usagef("%v", err)
	}
	if o.MaxBytes, err = a.int("max-bytes", 60000); err != nil {
		return ExitUsage, usagef("%v", err)
	}
	if o.MaxBytes < 1000 {
		return ExitUsage, usagef("--max-bytes must be at least 1000")
	}
	format := a.str("format")
	if format != "" && format != "md" && format != "json" {
		return ExitUsage, usagef("--format takes md or json")
	}

	ref := ""
	if a.has("current") {
		switch {
		case os.Getenv("PI_SESSION_FILE") != "":
			ref = os.Getenv("PI_SESSION_FILE")
		case os.Getenv("CURSOR_CONVERSATION_ID") != "":
			ref = "cursor:" + os.Getenv("CURSOR_CONVERSATION_ID")
		default:
			return ExitUsage, usagef("--current needs $PI_SESSION_FILE or $CURSOR_CONVERSATION_ID; neither is set")
		}
	} else {
		ref = a.pos[0]
	}
	if err := c.open(false); err != nil {
		return ExitError, err
	}
	defer c.close()
	s, code, err := c.resolveOne(ref)
	if err != nil {
		return code, err
	}
	src := c.sourceMap()[s.Agent]
	if src == nil {
		return ExitError, fmt.Errorf("no reader for agent %q", s.Agent)
	}
	session, entries, err := src.Parse(c.env, s.File)
	if err != nil {
		return ExitError, fmt.Errorf("read %s: %w", s.File, err)
	}
	session.File = s.File
	w := engine.Read(session, entries, o)
	if format == "json" {
		return ExitOK, writeJSON(c.stdout, w)
	}
	_, err = io.WriteString(c.stdout, w.Markdown())
	return ExitOK, err
}

// resolveOne finds exactly one session for ref.
func (c *cmd) resolveOne(ref string) (index.Session, int, error) {
	found, err := c.ix.Resolve(ref)
	if err != nil {
		return index.Session{}, ExitError, err
	}
	switch len(found) {
	case 0:
		return index.Session{}, ExitNoMatch, fmt.Errorf("no session matches %q", ref)
	case 1:
		return found[0], ExitOK, nil
	}
	if found[0].ID == strings.TrimPrefix(ref, found[0].Agent+":") && found[1].ID != found[0].ID {
		return found[0], ExitOK, nil // an exact id beats longer ids that share the prefix
	}
	var refs []string
	for _, s := range found {
		refs = append(refs, s.Ref())
	}
	return index.Session{}, ExitNoMatch, fmt.Errorf("%q matches several sessions: %s", ref, strings.Join(refs, ", "))
}

func (c *cmd) list(a *args) (int, error) {
	f, err := c.filter(a)
	if err != nil {
		return ExitUsage, err
	}
	if a.has("live") {
		f.Since = c.now.Add(-time.Hour)
	}
	limit, err := a.int("limit", 20)
	if err != nil {
		return ExitUsage, usagef("%v", err)
	}
	if err := c.open(false); err != nil {
		return ExitError, err
	}
	defer c.close()
	sessions, err := c.ix.List(f, limit)
	if err != nil {
		return ExitError, err
	}
	out := make([]map[string]any, len(sessions))
	for i, s := range sessions {
		out[i] = sessionJSON(s)
	}
	if err := writeJSON(c.stdout, map[string]any{"sessions": out}); err != nil {
		return ExitError, err
	}
	if len(out) == 0 {
		return ExitNoMatch, nil
	}
	return ExitOK, nil
}

func (c *cmd) resolve(a *args) (int, error) {
	if len(a.pos) != 1 {
		return ExitUsage, usagef("give one session ref")
	}
	if err := c.open(false); err != nil {
		return ExitError, err
	}
	defer c.close()
	s, code, err := c.resolveOne(a.pos[0])
	if err != nil {
		return code, err
	}
	return ExitOK, writeJSON(c.stdout, sessionJSON(s))
}

func (c *cmd) sources(a *args) (int, error) {
	if err := c.open(false); err != nil {
		return ExitError, err
	}
	defer c.close()
	counts, err := c.ix.AgentCounts()
	if err != nil {
		return ExitError, err
	}
	var agents []map[string]any
	for _, st := range c.sync {
		n := counts[st.Agent]
		agents = append(agents, map[string]any{"agent": st.Agent, "files": st.Files, "indexed": n[0], "failed": n[1]})
	}
	var size int64
	for _, suffix := range []string{"", "-wal"} {
		if info, err := os.Stat(c.ix.Path() + suffix); err == nil {
			size += info.Size()
		}
	}
	return ExitOK, writeJSON(c.stdout, map[string]any{
		"agents": agents,
		"index":  map[string]any{"path": c.ix.Path(), "bytes": size, "last_sync": c.ix.Meta("last_sync")},
	})
}

func (c *cmd) index(a *args) (int, error) {
	start := time.Now()
	if err := c.open(a.has("rebuild")); err != nil {
		return ExitError, err
	}
	defer c.close()
	return ExitOK, writeJSON(c.stdout, map[string]any{"agents": c.sync, "took_ms": time.Since(start).Milliseconds()})
}

func sessionJSON(s index.Session) map[string]any {
	m := map[string]any{"ref": s.Ref(), "agent": s.Agent, "id": s.ID, "file": s.File, "indexed_entries": s.Entries}
	for k, v := range map[string]string{"title": s.Title, "cwd": s.Cwd, "parent": s.Parent, "parse_error": s.ParseError} {
		if v != "" {
			m[k] = v
		}
	}
	if !s.Started.IsZero() {
		m["started"] = s.Started.Format(time.RFC3339)
	}
	if !s.Updated.IsZero() {
		m["updated"] = s.Updated.Format(time.RFC3339)
	}
	if len(s.Lacks) > 0 {
		m["lacks"] = s.Lacks
	}
	if s.Hidden {
		m["hidden"] = true
	}
	return m
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// pretty indents JSON output; it's off by default because agents read it
// and indentation costs tokens.
var pretty bool

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if pretty {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(v)
}
