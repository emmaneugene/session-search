// Package claude reads Claude Code sessions from ~/.claude/projects.
//
// Format facts (checked against real files; agentsview's claude parser is the
// reference):
//   - <project folder>/<session id>.jsonl is the session. Subagent transcripts
//     are <project folder>/<session id>/subagents/agent-<agentId>.jsonl with a
//     .meta.json beside them holding a description. Their records carry the
//     parent's sessionId.
//   - The file name is the session id. A resumed session can hold copied
//     records with another sessionId.
//   - Each content block of an assistant message is its own record (same
//     message.id). Tool results arrive in user records.
//   - Records form a tree through uuid and parentUuid. The last record with a
//     uuid is the leaf. After a compaction the boundary record has a null
//     parentUuid and names its parent in logicalParentUuid.
//   - Records without a uuid (file-history-snapshot, ai-title, custom-title,
//     last-prompt, ...) can sit anywhere, including last.
package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/emmaneugene/session-search/internal/model"
	"github.com/emmaneugene/session-search/internal/source"
)

// Source reads Claude Code sessions.
type Source struct{}

func (Source) Agent() string { return "claude" }

func (Source) Discover(env source.Env) ([]source.File, error) {
	root := env.Path(".claude", "projects")
	// Folders: <project>, <project>/<session id> and its subagents/. Other
	// folders (such as tool-results/) hold no transcripts.
	keep := func(path string) bool {
		if filepath.Base(path) == "subagents" {
			return true
		}
		return filepath.Dir(path) == root || filepath.Dir(filepath.Dir(path)) == root
	}
	var files []source.File
	err := env.Walk(root, keep, func(path string) error {
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		if f, ok := source.StatFile(path); ok {
			files = append(files, f)
		}
		return nil
	})
	return files, err
}

// part is one entry parsed from a line. toolID is the tool_use id of a
// tool_call or the tool_use_id a tool_result answers.
type part struct {
	kind   model.Kind
	text   string
	tool   string
	toolID string
}

// record is what one line holds.
type record struct {
	typ      string
	uuid     string
	parent   string
	msgID    string // assistant message id, shared by its block records
	time     time.Time
	parts    []part
	toolUses []string // ids of tool_use blocks
}

func parseLine(data []byte) (record, bool) {
	if !gjson.ValidBytes(data) {
		return record{}, false
	}
	v := gjson.ParseBytes(data)
	r := record{
		typ:    v.Get("type").String(),
		uuid:   v.Get("uuid").String(),
		parent: v.Get("parentUuid").String(),
		msgID:  v.Get("message.id").String(),
		time:   source.ParseTime(v.Get("timestamp").String()),
	}
	if r.parent == "" {
		r.parent = v.Get("logicalParentUuid").String()
	}
	switch r.typ {
	case "user":
		r.parts = userParts(v)
	case "assistant":
		r.parts = assistantParts(v.Get("message.content"))
		for _, p := range r.parts {
			if p.kind == model.KindToolCall {
				r.toolUses = append(r.toolUses, p.toolID)
			}
		}
	case "system":
		// Only local command output is conversation; turn timings, hook
		// summaries and compact boundaries are not.
		if v.Get("subtype").String() == "local_command" {
			if s := v.Get("content").String(); s != "" {
				r.parts = []part{{kind: model.KindInjected, text: s}}
			}
		}
	}
	return r, true
}

func userParts(v gjson.Result) []part {
	content := v.Get("message.content")
	meta := v.Get("isMeta").Bool()
	if v.Get("isCompactSummary").Bool() {
		if s := blocksText(content); s != "" {
			return []part{{kind: model.KindSummary, text: s}}
		}
		return nil
	}
	if content.Type == gjson.String {
		return userText(content.String(), meta)
	}
	var parts []part
	content.ForEach(func(_, b gjson.Result) bool {
		switch b.Get("type").String() {
		case "text":
			parts = append(parts, userText(b.Get("text").String(), meta)...)
		case "tool_result":
			// Content is a string or blocks; images and tool_reference
			// blocks have no text.
			if s := blocksText(b.Get("content")); s != "" {
				parts = append(parts, part{kind: model.KindToolResult, text: s, toolID: b.Get("tool_use_id").String()})
			}
		}
		return true
	})
	return parts
}

// injectedPrefixes start text that Claude Code wrote, not the user.
var injectedPrefixes = []string{
	"<system-reminder>", "<local-command-", "<task-notification>", "[Image:", "[Request interrupted",
}

func userText(s string, meta bool) []part {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	trimmed := strings.TrimSpace(s)
	if !meta && strings.Contains(trimmed, "<command-name>") {
		// A slash command with arguments is something the user typed.
		name, args := tag(trimmed, "command-name"), tag(trimmed, "command-args")
		if name != "" && args != "" {
			return []part{{kind: model.KindUser, text: name + " " + args}}
		}
		meta = true
	}
	if !meta {
		for _, p := range injectedPrefixes {
			if strings.HasPrefix(trimmed, p) {
				meta = true
				break
			}
		}
	}
	if meta {
		return []part{{kind: model.KindInjected, text: s}}
	}
	return []part{{kind: model.KindUser, text: s}}
}

// tag returns the trimmed text inside <name>...</name>.
func tag(s, name string) string {
	_, rest, ok := strings.Cut(s, "<"+name+">")
	if !ok {
		return ""
	}
	inner, _, _ := strings.Cut(rest, "</"+name+">")
	return strings.TrimSpace(inner)
}

func assistantParts(content gjson.Result) []part {
	var parts []part
	content.ForEach(func(_, b gjson.Result) bool {
		switch b.Get("type").String() {
		case "text":
			if s := b.Get("text").String(); s != "" {
				parts = append(parts, part{kind: model.KindAssistant, text: s})
			}
		case "thinking":
			// Often empty: only the signature is stored.
			if s := b.Get("thinking").String(); s != "" {
				parts = append(parts, part{kind: model.KindThinking, text: s})
			}
		case "tool_use":
			parts = append(parts, part{kind: model.KindToolCall, tool: b.Get("name").String(),
				text: compactJSON(b.Get("input")), toolID: b.Get("id").String()})
		}
		return true
	})
	return parts
}

// blocksText joins the text blocks of a string or block array.
func blocksText(c gjson.Result) string {
	if c.Type == gjson.String {
		return c.String()
	}
	var b strings.Builder
	c.ForEach(func(_, p gjson.Result) bool {
		if p.Get("type").String() == "text" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Get("text").String())
		}
		return true
	})
	return b.String()
}

func compactJSON(v gjson.Result) string {
	if !v.Exists() {
		return ""
	}
	if v.Type == gjson.String {
		return v.String()
	}
	return gjson.Get(v.Raw, "@ugly").Raw
}

func (Source) Parse(env source.Env, path string) (model.Session, []model.Entry, error) {
	s := model.Session{Agent: "claude", ID: strings.TrimSuffix(filepath.Base(path), ".jsonl"), File: path}
	sub := filepath.Base(filepath.Dir(path)) == "subagents"
	if sub {
		s.Parent = "claude:" + filepath.Base(filepath.Dir(filepath.Dir(path)))
	}
	type item struct {
		entry model.Entry
		line  int // index into lines
	}
	var (
		items         []item
		lines         []record
		parent        = map[string]string{}
		lineOf        = map[string]int{}
		leaf          string
		customTitle   string
		aiTitle       string
		firstUserText string
	)
	err := source.EachLine(path, func(l source.Line) error {
		r, ok := parseLine(l.Data)
		if !ok {
			return nil
		}
		v := gjson.ParseBytes(l.Data)
		switch r.typ {
		case "custom-title":
			customTitle = v.Get("customTitle").String()
		case "ai-title":
			aiTitle = v.Get("aiTitle").String()
		}
		if s.Cwd == "" {
			s.Cwd = v.Get("cwd").String()
		}
		if !r.time.IsZero() {
			if s.Started.IsZero() || r.time.Before(s.Started) {
				s.Started = r.time
			}
			if r.time.After(s.Updated) {
				s.Updated = r.time
			}
		}
		if r.uuid != "" {
			parent[r.uuid] = r.parent
			lineOf[r.uuid] = len(lines)
			leaf = r.uuid
		}
		for i, p := range r.parts {
			if p.kind == model.KindUser && firstUserText == "" {
				firstUserText = p.text
			}
			items = append(items, item{
				entry: model.Entry{Time: r.time, Kind: p.kind, Text: p.text, Tool: p.tool, Loc: source.LineLoc(l, i)},
				line:  len(lines),
			})
		}
		lines = append(lines, r)
		return nil
	})
	if err != nil {
		return s, nil, err
	}

	// Entries on the path from the leaf to the root are the live conversation.
	// Two kinds of off-path record are still live: the other blocks of an
	// assistant message on the path, and results of parallel tool calls
	// issued by an assistant message on the path. Everything else belongs to a
	// branch the user rewound from.
	active := map[string]bool{}
	msgs, toolUses := map[string]bool{}, map[string]bool{}
	for id, n := leaf, 0; id != "" && n <= len(parent); id, n = parent[id], n+1 {
		active[id] = true
		r := lines[lineOf[id]]
		if r.msgID != "" {
			msgs[r.msgID] = true
		}
		for _, t := range r.toolUses {
			toolUses[t] = true
		}
	}
	for _, r := range lines {
		if r.typ == "assistant" && msgs[r.msgID] {
			for _, t := range r.toolUses {
				toolUses[t] = true
			}
		}
	}
	live := func(r record, parts []part) bool {
		if r.uuid == "" || active[r.uuid] {
			return true
		}
		if r.typ == "assistant" {
			return msgs[r.msgID]
		}
		for _, p := range parts {
			if p.kind != model.KindToolResult || !toolUses[p.toolID] {
				return false
			}
		}
		return len(parts) > 0
	}

	// Tool results name their tool through the tool_use that precedes them.
	toolName := map[string]string{}
	entries := make([]model.Entry, len(items))
	for i, it := range items {
		r := lines[it.line]
		p := r.parts[it.entry.Loc.Part]
		e := it.entry
		e.Seq = i
		if p.kind == model.KindToolCall {
			toolName[p.toolID] = p.tool
		}
		if p.kind == model.KindToolResult {
			e.Tool = toolName[p.toolID]
		}
		e.Abandoned = !live(r, r.parts)
		entries[i] = e
	}

	if sub {
		s.Title = metaDescription(path)
	}
	switch {
	case s.Title != "":
	case customTitle != "":
		s.Title = customTitle
	case aiTitle != "":
		s.Title = aiTitle
	default:
		s.Title = firstUserText
	}
	s.Title = source.TitleFrom(s.Title)
	s.Hidden = !sub && firstUserText == ""
	return s, entries, nil
}

// metaDescription reads agent-<id>.meta.json beside a subagent transcript.
func metaDescription(path string) string {
	data, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".meta.json")
	if err != nil {
		return ""
	}
	return gjson.GetBytes(data, "description").String()
}

func (Source) Text(env source.Env, path string, loc model.Loc) (string, error) {
	data, err := source.ReadLine(path, loc.Offset, loc.Length)
	if err != nil {
		return "", err
	}
	r, ok := parseLine(data)
	if !ok || loc.Part >= len(r.parts) {
		return "", fmt.Errorf("claude: no entry %d at offset %d of %s", loc.Part, loc.Offset, path)
	}
	return r.parts[loc.Part].text, nil
}
