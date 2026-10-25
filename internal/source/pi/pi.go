// Package pi reads Pi coding agent sessions from ~/.pi/agent/sessions.
//
// Format: https://github.com/earendil-works/pi docs/session-format.md. Each
// file is JSONL. Line 1 is a "session" header with id, cwd and an optional
// parentSession. Other lines are tree entries linked by id and parentId; the
// last entry in the file is the current leaf, and entries off the path from
// the leaf to the root belong to branches the user left. Subagent sessions
// sit in <cwd folder>/subagents/<parent session id>/.
package pi

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/emmaneugene/session-search/internal/model"
	"github.com/emmaneugene/session-search/internal/source"
)

// Source reads Pi sessions.
type Source struct{}

func (Source) Agent() string { return "pi" }

func root(env source.Env) string { return env.Path(".pi", "agent", "sessions") }

func (Source) Discover(env source.Env) ([]source.File, error) {
	var files []source.File
	err := env.Walk(root(env), nil, func(path string) error {
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

// part is one entry parsed from a line, before Seq and branch are known.
type part struct {
	kind model.Kind
	text string
	tool string
}

// record is what one line holds.
type record struct {
	typ      string
	id       string
	parentID string
	time     time.Time
	parts    []part
}

func parseLine(data []byte) (record, bool) {
	if !gjson.ValidBytes(data) {
		return record{}, false
	}
	v := gjson.ParseBytes(data)
	r := record{
		typ:      v.Get("type").String(),
		id:       v.Get("id").String(),
		parentID: v.Get("parentId").String(),
		time:     source.ParseTime(v.Get("timestamp").String()),
	}
	switch r.typ {
	case "message":
		r.parts = messageParts(v.Get("message"))
	case "compaction", "branch_summary":
		if s := v.Get("summary").String(); s != "" {
			r.parts = []part{{kind: model.KindSummary, text: s}}
		}
	case "custom_message":
		if s := contentText(v.Get("content")); s != "" {
			r.parts = []part{{kind: model.KindInjected, text: s}}
		}
	}
	return r, true
}

func messageParts(m gjson.Result) []part {
	switch m.Get("role").String() {
	case "user":
		if s := contentText(m.Get("content")); s != "" {
			return []part{{kind: model.KindUser, text: s}}
		}
	case "assistant":
		var parts []part
		m.Get("content").ForEach(func(_, c gjson.Result) bool {
			switch c.Get("type").String() {
			case "text":
				if s := c.Get("text").String(); s != "" {
					parts = append(parts, part{kind: model.KindAssistant, text: s})
				}
			case "thinking":
				if s := c.Get("thinking").String(); s != "" {
					parts = append(parts, part{kind: model.KindThinking, text: s})
				}
			case "toolCall":
				parts = append(parts, part{kind: model.KindToolCall, tool: c.Get("name").String(), text: compactJSON(c.Get("arguments"))})
			}
			return true
		})
		return parts
	case "toolResult":
		if s := contentText(m.Get("content")); s != "" {
			return []part{{kind: model.KindToolResult, tool: m.Get("toolName").String(), text: s}}
		}
	case "bashExecution":
		parts := []part{{kind: model.KindToolCall, tool: "bash", text: m.Get("command").String()}}
		if s := m.Get("output").String(); s != "" {
			parts = append(parts, part{kind: model.KindToolResult, tool: "bash", text: s})
		}
		return parts
	case "custom":
		if s := contentText(m.Get("content")); s != "" {
			return []part{{kind: model.KindInjected, text: s}}
		}
	}
	// "system" messages hold the system prompt and tool list: not indexed.
	return nil
}

// contentText joins the text blocks of a string or content-block array,
// skipping images.
func contentText(c gjson.Result) string {
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
	s := model.Session{Agent: "pi", File: path}
	type owned struct {
		entry model.Entry
		owner string // id of the tree entry the line belongs to
	}
	var (
		items   []owned
		parent  = map[string]string{}
		leaf    string
		first   = true
		titled  bool
		userTxt string
	)
	err := source.EachLine(path, func(l source.Line) error {
		if first {
			first = false
			v := gjson.ParseBytes(l.Data)
			if v.Get("type").String() == "session" {
				s.ID = v.Get("id").String()
				s.Cwd = v.Get("cwd").String()
				s.Started = source.ParseTime(v.Get("timestamp").String())
				if p := v.Get("parentSession").String(); p != "" {
					s.Parent = "pi:" + idFromFilename(p)
				}
				return nil
			}
		}
		r, ok := parseLine(l.Data)
		if !ok {
			return nil
		}
		if r.id != "" {
			parent[r.id] = r.parentID
			leaf = r.id
		}
		if !r.time.IsZero() && r.time.After(s.Updated) {
			s.Updated = r.time
		}
		if r.typ == "session_info" {
			if name := gjson.GetBytes(l.Data, "name").String(); name != "" {
				s.Title = source.TitleFrom(name)
				titled = true
			}
		}
		for i, p := range r.parts {
			if p.kind == model.KindUser && userTxt == "" {
				userTxt = p.text
			}
			items = append(items, owned{
				entry: model.Entry{Time: r.time, Kind: p.kind, Text: p.text, Tool: p.tool, Loc: source.LineLoc(l, i)},
				owner: r.id,
			})
		}
		return nil
	})
	if err != nil {
		return s, nil, err
	}
	if s.ID == "" {
		s.ID = idFromFilename(path)
	}
	if sub := subagentParent(path); sub != "" {
		s.Parent = "pi:" + sub
	}
	if !titled {
		s.Title = source.TitleFrom(userTxt)
	}
	if s.Updated.IsZero() {
		s.Updated = s.Started
	}

	active := map[string]bool{}
	for id, n := leaf, 0; id != "" && n <= len(parent); id, n = parent[id], n+1 {
		active[id] = true
	}
	entries := make([]model.Entry, len(items))
	for i, it := range items {
		e := it.entry
		e.Seq = i
		e.Abandoned = it.owner != "" && !active[it.owner]
		entries[i] = e
	}
	return s, entries, nil
}

func (Source) Text(env source.Env, path string, loc model.Loc) (string, error) {
	data, err := source.ReadLine(path, loc.Offset, loc.Length)
	if err != nil {
		return "", err
	}
	r, ok := parseLine(data)
	if !ok || loc.Part >= len(r.parts) {
		return "", fmt.Errorf("pi: no entry %d at offset %d of %s", loc.Part, loc.Offset, path)
	}
	return r.parts[loc.Part].text, nil
}

// idFromFilename returns the id in "<timestamp>_<id>.jsonl".
func idFromFilename(p string) string {
	base := strings.TrimSuffix(filepath.Base(p), ".jsonl")
	if _, id, ok := strings.Cut(base, "_"); ok {
		return id
	}
	return base
}

// subagentParent returns <parent id> for .../subagents/<parent id>/<file>.
func subagentParent(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(filepath.Dir(dir)) == "subagents" {
		return filepath.Base(dir)
	}
	return ""
}
