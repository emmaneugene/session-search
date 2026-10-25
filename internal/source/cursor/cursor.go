// Package cursor reads Cursor agent chats. One chat is one session, built
// from three stores:
//
//   - The transcript, ~/.cursor/projects/<folder>/agent-transcripts/<id>/<id>.jsonl
//     (or <id>.jsonl in the older flat layout), with subagents and side
//     chats in <id>/subagents/<sub>.jsonl. It holds user text, assistant
//     text and tool calls, in order, but no tool results and no times other
//     than a <timestamp> tag per user turn. It is the base of the session.
//   - The editor database (see db.go), which adds the title, workspace,
//     times and tool results for chats run in the editor.
//   - ~/.config/cursor/chats/<md5 of cwd>/<id>/meta.json, with the cwd and
//     times of command-line chats.
package cursor

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/emmaneugene/session-search/internal/model"
	"github.com/emmaneugene/session-search/internal/source"
)

// Source reads Cursor chats.
type Source struct{}

func (Source) Agent() string { return "cursor" }

func projectsRoot(env source.Env) string { return env.Path(".cursor", "projects") }

func chatID(path string) string { return strings.TrimSuffix(filepath.Base(path), ".jsonl") }

func (Source) Discover(env source.Env) ([]source.File, error) {
	root := projectsRoot(env)
	projects, err := env.ReadDir(root)
	if err != nil {
		return nil, err
	}
	// A chat id can have a transcript in two project folders (the chat was
	// moved to another window, or copied under another chat as a side
	// chat). Keep the most recently written one.
	type found struct {
		path string
		info os.FileInfo
	}
	best := map[string]found{}
	add := func(p string) {
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			return
		}
		id := chatID(p)
		if old, ok := best[id]; ok && !info.ModTime().After(old.info.ModTime()) {
			return
		}
		best[id] = found{p, info}
	}
	for _, proj := range projects {
		at := filepath.Join(root, proj.Name, "agent-transcripts")
		if info, err := os.Stat(at); err != nil || !info.IsDir() {
			continue
		}
		chats, err := env.ReadDir(at)
		if err != nil {
			return nil, err
		}
		for _, c := range chats {
			if isSDKRun(c.Name) {
				continue
			}
			if !c.IsDir {
				if strings.HasSuffix(c.Name, ".jsonl") {
					add(filepath.Join(at, c.Name))
				}
				continue
			}
			add(filepath.Join(at, c.Name, c.Name+".jsonl"))
			sub := filepath.Join(at, c.Name, "subagents")
			if info, err := os.Stat(sub); err != nil || !info.IsDir() {
				continue
			}
			subs, err := env.ReadDir(sub)
			if err != nil {
				return nil, err
			}
			for _, s := range subs {
				if !s.IsDir && strings.HasSuffix(s.Name, ".jsonl") {
					add(filepath.Join(sub, s.Name))
				}
			}
		}
	}
	var marks map[string]string
	if len(best) > 0 {
		marks = markers(env)
	}
	files := make([]source.File, 0, len(best))
	for id, f := range best {
		fp := source.StatFingerprint(f.info)
		if m, ok := marks[id]; ok {
			fp += ":db=" + m
		}
		files = append(files, source.File{Path: f.path, Fingerprint: fp})
	}
	slices.SortFunc(files, func(a, b source.File) int { return strings.Compare(a.Path, b.Path) })
	return files, nil
}

// item is a transcript entry before Seq is known, with the record it came
// from.
type item struct {
	entry model.Entry
	rec   int
}

func (Source) Parse(env source.Env, path string) (model.Session, []model.Entry, error) {
	id := chatID(path)
	s := model.Session{Agent: "cursor", ID: id, File: path}
	info, err := os.Stat(path)
	if err != nil {
		return s, nil, err
	}

	items, recs, err := readTranscript(path)
	if err != nil {
		return s, nil, err
	}

	c, inDB := loadChat(env, id)
	if inDB && len(c.bubbles) == 0 {
		inDB = false
	}
	entries, matched := merge(items, recs, c.bubbles)
	for i := range entries {
		entries[i].Seq = i
	}

	// Parent: the subagents folder, else the database's side-chat link.
	if dir := filepath.Dir(path); filepath.Base(dir) == "subagents" {
		s.Parent = "cursor:" + filepath.Base(filepath.Dir(dir))
	} else if p := c.composer.Get("subagentInfo.parentComposerId").String(); p != "" {
		s.Parent = "cursor:" + p
	}

	var userText string
	var first, last time.Time
	for _, e := range entries {
		if e.Kind == model.KindUser && userText == "" {
			userText = e.Text
		}
		if !e.Time.IsZero() {
			if first.IsZero() {
				first = e.Time
			}
			last = e.Time
		}
	}
	s.Title = c.composer.Get("name").String()
	if s.Title == "" {
		s.Title = source.TitleFrom(userText)
	}
	meta := cliMeta(env, id)
	s.Started = firstTime(source.MillisTime(c.composer.Get("createdAt").Int()), source.MillisTime(meta.Get("createdAtMs").Int()), first)
	s.Updated = firstTime(source.MillisTime(c.composer.Get("lastUpdatedAt").Int()), source.MillisTime(meta.Get("updatedAtMs").Int()), last, info.ModTime().UTC())
	// A configPath names a .code-workspace file, not a folder, so only a
	// single-folder workspace's uri gives the cwd here.
	s.Cwd = c.composer.Get("workspaceIdentifier.uri.fsPath").String()
	if s.Cwd == "" {
		s.Cwd = meta.Get("cwd").String()
	}
	if s.Cwd == "" {
		folder := projectFolder(env, path)
		if s.Cwd = decodeFolder(env, folder); s.Cwd == "" {
			s.Cwd = folder
		}
	}

	hasCalls := slices.ContainsFunc(entries, func(e model.Entry) bool { return e.Kind == model.KindToolCall })
	if !inDB || (hasCalls && matched == 0) {
		s.Lacks = append(s.Lacks, model.LackToolResults)
	}
	if first.IsZero() {
		s.Lacks = append(s.Lacks, model.LackTimestamps)
	}
	s.Hidden = userText == ""
	return s, entries, nil
}

func readTranscript(path string) (items []item, recs []record, err error) {
	err = source.EachLine(path, func(l source.Line) error {
		r, ok := parseLine(l.Data)
		if !ok || r.role == "" {
			return nil
		}
		if r.boundary {
			// A side chat: what came before is the parent's history.
			items, recs = nil, nil
		}
		for i, p := range r.parts {
			items = append(items, item{
				entry: model.Entry{Kind: p.kind, Text: p.text, Tool: p.tool, Loc: source.LineLoc(l, i)},
				rec:   len(recs),
			})
		}
		recs = append(recs, r)
		return nil
	})
	return items, recs, err
}

func firstTime(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func projectFolder(env source.Env, path string) string {
	rel, err := filepath.Rel(projectsRoot(env), path)
	if err != nil {
		return ""
	}
	return strings.Split(rel, string(filepath.Separator))[0]
}

// cliMeta finds <hash>/<id>/meta.json under ~/.config/cursor/chats. The
// hash is the md5 of the cwd, which is what we're looking for, so try each.
func cliMeta(env source.Env, id string) gjson.Result {
	root := env.Path(".config", "cursor", "chats")
	hashes, _ := env.ReadDir(root)
	for _, h := range hashes {
		data, err := os.ReadFile(filepath.Join(root, h.Name, id, "meta.json"))
		if err == nil && gjson.ValidBytes(data) {
			return gjson.ParseBytes(data)
		}
	}
	return gjson.Result{}
}

// merge adds database times and tool results to the transcript entries,
// and returns how many tool calls it matched to a database tool bubble.
func merge(items []item, recs []record, bubbles []bubble) ([]model.Entry, int) {
	userAt := anchorUsers(items, bubbles)
	match := matchCalls(items, bubbles, userAt)

	recTime := make([]time.Time, len(recs))
	for i, r := range recs {
		recTime[i] = r.when
	}
	for i, j := range userAt {
		recTime[items[i].rec] = bubbles[j].created
	}
	// Entries without a time of their own take the latest time seen so far,
	// so times never go backwards.
	var out []model.Entry
	var clock time.Time
	for i, it := range items {
		if t := recTime[it.rec]; t.After(clock) {
			clock = t
		}
		e := it.entry
		e.Time = clock
		j, ok := match[i]
		if !ok {
			out = append(out, e)
			continue
		}
		b := bubbles[j]
		e.Time = firstTime(b.created, clock)
		out = append(out, e)
		done := firstTime(b.done, b.created, clock)
		if done.After(clock) {
			clock = done
		}
		if b.result != "" {
			out = append(out, model.Entry{Time: done, Kind: model.KindToolResult, Text: b.result, Tool: e.Tool, Loc: model.Loc{Key: b.key}})
		}
	}
	return out, len(match)
}

// anchorUsers maps user entries to the database user bubble with the same
// text, in order. Entries that don't match (Cursor's own follow-up prompts
// are often missing from the database) stay unanchored.
func anchorUsers(items []item, bubbles []bubble) map[int]int {
	userAt := map[int]int{}
	next := 0
	for i, it := range items {
		if it.entry.Kind != model.KindUser {
			continue
		}
		for j := next; j < len(bubbles); j++ {
			if bubbles[j].user && strings.TrimSpace(bubbles[j].text) == it.entry.Text {
				userAt[i], next = j, j+1
				break
			}
		}
	}
	return userAt
}

// matchCalls pairs transcript tool calls (item index) with database tool
// bubbles (bubble index).
//
// Transcript tool calls have no ids, and the database names tools
// differently, stores parallel calls in another order, and can miss a call
// or hold one the transcript lacks. So anchored user turns split both sides,
// and calls only match bubbles in their own turn. Within a turn, a first
// pass matches each call to the first free bubble with a compatible name
// that shares an argument value (a command, path or pattern). A second pass
// matches the rest by name alone, but only between the bubbles of their
// matched neighbours, so one missing bubble can't shift the calls after it.
func matchCalls(items []item, bubbles []bubble, userAt map[int]int) map[int]int {
	match := map[int]int{}
	used := make([]bool, len(bubbles))
	free := func(i, j int) bool {
		return !used[j] && bubbles[j].tool != "" && sameTool(items[i].entry.Tool, bubbles[j].tool)
	}
	take := func(i, j int) { match[i], used[j] = j, true }

	var calls []int
	lo := 0
	flush := func(hi int) {
		for _, i := range calls {
			args := leaves(items[i].entry.Text)
			for j := lo; j < hi; j++ {
				if free(i, j) && shares(args, bubbles[j].args) {
					take(i, j)
					break
				}
			}
		}
		for k, i := range calls {
			if _, ok := match[i]; ok {
				continue
			}
			from, to := lo, hi
			for _, p := range calls[:k] {
				if j, ok := match[p]; ok && j >= from {
					from = j + 1
				}
			}
			for _, n := range calls[k+1:] {
				if j, ok := match[n]; ok {
					to = min(to, j)
					break
				}
			}
			for j := from; j < to; j++ {
				if free(i, j) {
					take(i, j)
					break
				}
			}
		}
		calls = nil
	}
	for i, it := range items {
		if j, ok := userAt[i]; ok {
			flush(j)
			lo = j + 1
		}
		if it.entry.Kind == model.KindToolCall {
			calls = append(calls, i)
		}
	}
	flush(len(bubbles))
	return match
}

// leaves collects the string values of at least 3 characters in a JSON
// document, at any depth.
func leaves(raw string) map[string]bool {
	out := map[string]bool{}
	var walk func(v gjson.Result)
	walk = func(v gjson.Result) {
		switch {
		case v.IsObject() || v.IsArray():
			v.ForEach(func(_, x gjson.Result) bool { walk(x); return true })
		case v.Type == gjson.String && len(v.Str) >= 3:
			out[v.Str] = true
		}
	}
	if gjson.Valid(raw) {
		walk(gjson.Parse(raw))
	}
	return out
}

func shares(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// dbTools maps transcript tool names to the names the database stores.
var dbTools = map[string][]string{
	"Shell":               {"run_terminal_command_v2", "run_terminal_command"},
	"Read":                {"read_file_v2", "read_file"},
	"Grep":                {"ripgrep_raw_search", "grep_search", "grep"},
	"Glob":                {"glob_file_search"},
	"StrReplace":          {"edit_file_v2", "edit_file", "search_replace"},
	"Write":               {"edit_file_v2", "edit_file", "write"},
	"Delete":              {"delete_file"},
	"AskQuestion":         {"ask_question"},
	"AwaitShell":          {"await"},
	"GetDynamicTools":     {"get_mcp_tools"},
	"GetMcpTools":         {"get_mcp_tools"},
	"SearchConversations": {"search_conversations"},
	"SemanticSearch":      {"semantic_search_full", "codebase_search"},
	"Task":                {"task_v2", "task"},
	"TodoWrite":           {"todo_write"},
	"UpdateCurrentStep":   {"update_current_step"},
	"WebFetch":            {"web_fetch"},
	"WebSearch":           {"web_search"},
}

func sameTool(transcript, db string) bool {
	switch transcript {
	case "CallDynamicTool", "CallMcpTool":
		// The database stores the tool the wrapper called (mcp-..., web_fetch, task_v2).
		return true
	}
	if names, ok := dbTools[transcript]; ok {
		return slices.Contains(names, db)
	}
	return strings.HasPrefix(db, snake(transcript))
}

func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (Source) Text(env source.Env, path string, loc model.Loc) (string, error) {
	if loc.Key != "" {
		return bubbleResult(env, loc.Key)
	}
	data, err := source.ReadLine(path, loc.Offset, loc.Length)
	if err != nil {
		return "", err
	}
	r, ok := parseLine(data)
	if !ok || loc.Part >= len(r.parts) {
		return "", fmt.Errorf("cursor: no entry %d at offset %d of %s", loc.Part, loc.Offset, path)
	}
	return r.parts[loc.Part].text, nil
}

// isSDKRun reports a transcript written by the Cursor SDK (agent-<uuid>, as a
// folder or a flat file). Pi's Cursor provider drives the SDK, and the Pi
// session file holds the same conversation with more detail, so these runs
// aren't Cursor sessions and are never indexed.
func isSDKRun(name string) bool {
	return strings.HasPrefix(name, "agent-")
}
