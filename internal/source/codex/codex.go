// Package codex reads Codex sessions from ~/.codex/sessions and
// ~/.codex/archived_sessions.
//
// Every line is {timestamp, type, payload}. The session id is the first
// session_meta's payload.id, not the file name; a legacy fork may hold a second
// session_meta for its parent. response_item carries the model's conversation
// (messages, reasoning, tool calls and outputs). event_msg repeats the user and
// assistant messages and is only complete in older files, so it is a fallback
// for files with no response_item message. Titles live in session_index.jsonl,
// which a rename updates without touching the session file.
package codex

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

// Source reads Codex sessions.
type Source struct{}

func (Source) Agent() string { return "codex" }

func indexPath(env source.Env) string { return env.Path(".codex", "session_index.jsonl") }

func (Source) Discover(env source.Env) ([]source.File, error) {
	// A rename rewrites only the index, so its stat is part of every fingerprint.
	var indexMark string
	if info, err := os.Stat(indexPath(env)); err == nil {
		indexMark = "+" + source.StatFingerprint(info)
	}
	var files []source.File
	for _, dir := range []string{"sessions", "archived_sessions"} {
		err := env.Walk(env.Path(".codex", dir), nil, func(path string) error {
			if !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			if f, ok := source.StatFile(path); ok {
				f.Fingerprint += indexMark
				files = append(files, f)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

// part is one entry parsed from a line, before Seq is known.
type part struct {
	kind   model.Kind
	text   string
	tool   string
	callID string
	// event marks parts from the event_msg stream: the fallback.
	event bool
	// message marks user and assistant text from response_item.
	message bool
}

// Text that opens an injected context block in a user turn.
var injectedPrefixes = []string{
	"# AGENTS.md instructions", "<environment_context>", "<user_instructions>",
	"<skill>", "<permissions instructions>", "<app-context>", "<turn_aborted>",
	"<subagent_notification>", "<collaboration_mode>", "<user_action>",
	"<INSTRUCTIONS>", "<EXTREMELY_IMPORTANT>", "<recent_conversation>",
	"<recommended_plugins>", "<guardian_tool_descriptions>", "<external_codex_apps_open_page>",
	"<multi_agent_mode>", "<multi_agent_role>", "<image ", "</image>",
}

func isInjected(text string) bool {
	t := strings.TrimSpace(text)
	for _, p := range injectedPrefixes {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}

// parseLine returns the entries one line holds. It depends on the line alone,
// so Text can rebuild any entry from its Loc.
func parseLine(v gjson.Result) []part {
	p := v.Get("payload")
	switch v.Get("type").String() {
	case "response_item":
		return itemParts(p)
	case "event_msg":
		switch p.Get("type").String() {
		case "user_message":
			return eventPart(model.KindUser, p.Get("message").String())
		case "agent_message":
			return eventPart(model.KindAssistant, p.Get("message").String())
		case "agent_reasoning":
			return eventPart(model.KindThinking, p.Get("text").String())
		}
	case "compacted":
		// replacement_history replays earlier messages: not entries.
		if s := p.Get("message").String(); s != "" {
			return []part{{kind: model.KindSummary, text: s}}
		}
	}
	return nil
}

func eventPart(k model.Kind, text string) []part {
	if text == "" {
		return nil
	}
	return []part{{kind: k, text: text, event: true}}
}

func itemParts(p gjson.Result) []part {
	switch p.Get("type").String() {
	case "message":
		role := p.Get("role").String()
		var parts []part
		p.Get("content").ForEach(func(_, c gjson.Result) bool {
			text := c.Get("text").String()
			if text == "" { // images and encrypted blocks have no text
				return true
			}
			switch {
			case role == "assistant":
				parts = append(parts, part{kind: model.KindAssistant, text: text, message: true})
			case role == "user" && !isInjected(text):
				parts = append(parts, part{kind: model.KindUser, text: text, message: true})
			default: // developer, and injected context in a user turn
				parts = append(parts, part{kind: model.KindInjected, text: text})
			}
			return true
		})
		return parts
	case "agent_message": // inter-agent message; its payload is usually encrypted
		if s := blocksText(p.Get("content")); s != "" {
			return []part{{kind: model.KindInjected, text: s}}
		}
	case "reasoning":
		var texts []string
		p.Get("summary").ForEach(func(_, c gjson.Result) bool {
			if s := c.Get("text").String(); s != "" {
				texts = append(texts, s)
			}
			return true
		})
		if s := strings.Join(texts, "\n\n"); s != "" {
			return []part{{kind: model.KindThinking, text: s}}
		}
	case "function_call":
		return call(p, p.Get("name").String(), compact(p.Get("arguments")))
	case "custom_tool_call":
		return call(p, p.Get("name").String(), p.Get("input").String())
	case "tool_search_call":
		return call(p, "tool_search", compact(p.Get("arguments")))
	case "local_shell_call":
		var args []string
		p.Get("action.command").ForEach(func(_, a gjson.Result) bool {
			args = append(args, a.String())
			return true
		})
		return call(p, "local_shell", strings.Join(args, " "))
	case "web_search_call":
		q := p.Get("action.query").String()
		if q == "" {
			q = p.Get("action.queries.0").String()
		}
		if q == "" {
			return nil
		}
		return []part{{kind: model.KindToolCall, tool: "web_search", text: q}}
	case "function_call_output", "custom_tool_call_output":
		if s := blocksText(p.Get("output")); s != "" {
			return []part{{kind: model.KindToolResult, text: s, callID: p.Get("call_id").String()}}
		}
	case "tool_search_output":
		var names []string
		var walk func(tools gjson.Result)
		walk = func(tools gjson.Result) {
			tools.ForEach(func(_, t gjson.Result) bool {
				if n := t.Get("name").String(); n != "" {
					names = append(names, n)
				}
				walk(t.Get("tools"))
				return true
			})
		}
		walk(p.Get("tools"))
		if len(names) > 0 {
			return []part{{kind: model.KindToolResult, text: strings.Join(names, "\n"), callID: p.Get("call_id").String()}}
		}
	}
	return nil
}

func call(p gjson.Result, tool, text string) []part {
	return []part{{kind: model.KindToolCall, tool: tool, text: text, callID: p.Get("call_id").String()}}
}

// blocksText returns a string as is, or joins the text blocks of an array and
// skips images and encrypted blocks.
func blocksText(c gjson.Result) string {
	if c.Type == gjson.String {
		return c.String()
	}
	var texts []string
	c.ForEach(func(_, b gjson.Result) bool {
		if s := b.Get("text").String(); s != "" {
			texts = append(texts, s)
		}
		return true
	})
	return strings.Join(texts, "\n")
}

// compact re-encodes JSON arguments without whitespace. Arguments arrive as a
// JSON string inside a string, or as an object; anything else stays as is.
func compact(v gjson.Result) string {
	s := v.Raw
	if v.Type == gjson.String {
		s = v.String()
	}
	if !gjson.Valid(s) {
		return s
	}
	return gjson.Get(s, "@ugly").Raw
}

// replayGap is the pause that ends a fork's replayed parent history. The
// replay is written in a burst of milliseconds; the fork's own turns follow
// after a real pause.
const replayGap = 250 * time.Millisecond

type item struct {
	time time.Time
	part part
	loc  model.Loc
}

func (Source) Parse(env source.Env, path string) (model.Session, []model.Entry, error) {
	s := model.Session{Agent: "codex", File: path}
	var (
		items       []item
		first       = true
		fork        bool
		replay      bool
		prev        time.Time
		hasMessages bool
		review      bool
		turnCwd     string
		nickname    string // subagents: their task arrives encrypted, so no user text
	)
	err := source.EachLine(path, func(l source.Line) error {
		if !gjson.ValidBytes(l.Data) {
			return nil
		}
		v := gjson.ParseBytes(l.Data)
		ts := source.ParseTime(v.Get("timestamp").String())
		if ts.After(s.Updated) {
			s.Updated = ts
		}
		p := v.Get("payload")
		typ := v.Get("type").String()
		if typ == "session_meta" {
			if first { // a second one describes the parent of a legacy fork
				first = false
				s.ID = p.Get("id").String()
				s.Cwd = p.Get("cwd").String()
				s.Started = source.ParseTime(p.Get("timestamp").String())
				if from := p.Get("forked_from_id").String(); from != "" {
					fork, replay = true, true
					s.Parent = "codex:" + from
				} else if from := p.Get("parent_thread_id").String(); from != "" {
					s.Parent = "codex:" + from
				}
				review = p.Get("thread_source").String() == "guardian_review"
				nickname = strings.TrimSpace(p.Get("agent_nickname").String() + " " + p.Get("agent_path").String())
				prev = ts
			}
			return nil
		}
		if fork && replay {
			if !ts.IsZero() && !prev.IsZero() && ts.Sub(prev) > replayGap {
				replay, items, hasMessages = false, nil, false
			}
			prev = ts
		}
		switch typ {
		case "turn_context":
			if turnCwd == "" {
				turnCwd = p.Get("cwd").String()
			}
			review = review || p.Get("model").String() == "codex-auto-review"
		case "event_msg":
			if p.Get("type").String() == "thread_settings_applied" {
				review = review || p.Get("thread_settings.model").String() == "codex-auto-review"
			}
		}
		for i, pt := range parseLine(v) {
			hasMessages = hasMessages || pt.message
			items = append(items, item{time: ts, part: pt, loc: source.LineLoc(l, i)})
		}
		return nil
	})
	if err != nil {
		return s, nil, err
	}
	if s.ID == "" {
		s.ID = idFromFilename(path)
	}
	if s.Cwd == "" {
		s.Cwd = turnCwd
	}
	if s.Updated.IsZero() {
		s.Updated = s.Started
	}

	// The two streams repeat each other. With response_item messages present,
	// drop every event part. Without them, keep the event parts and drop the
	// response_item reasoning, which the event stream also covers.
	var entries []model.Entry
	tools := map[string]string{} // call_id -> tool name
	var firstUser string
	conversation := false
	for _, it := range items {
		pt := it.part
		if pt.event && hasMessages || !hasMessages && !pt.event && pt.kind == model.KindThinking {
			continue
		}
		if pt.callID != "" {
			if pt.kind == model.KindToolCall {
				tools[pt.callID] = pt.tool
			} else {
				pt.tool = tools[pt.callID]
			}
		}
		if pt.kind == model.KindUser || pt.kind == model.KindAssistant {
			conversation = true
		}
		if pt.kind == model.KindUser && firstUser == "" {
			firstUser = pt.text
		}
		entries = append(entries, model.Entry{Seq: len(entries), Time: it.time, Kind: pt.kind, Text: pt.text, Tool: pt.tool, Loc: it.loc})
	}
	if firstUser == "" {
		firstUser = nickname
	}
	s.Hidden = review || !conversation
	s.Title = titles(env)[s.ID]
	if s.Title == "" {
		s.Title = source.TitleFrom(firstUser)
	} else {
		s.Title = source.TitleFrom(s.Title)
	}
	return s, entries, nil
}

// titles reads session_index.jsonl: {"id","thread_name","updated_at"} per
// line, and the last line for an id wins.
func titles(env source.Env) map[string]string {
	out := map[string]string{}
	_ = source.EachLine(indexPath(env), func(l source.Line) error {
		v := gjson.ParseBytes(l.Data)
		if id := v.Get("id").String(); id != "" {
			out[id] = v.Get("thread_name").String()
		}
		return nil
	})
	return out
}

func (Source) Text(env source.Env, path string, loc model.Loc) (string, error) {
	data, err := source.ReadLine(path, loc.Offset, loc.Length)
	if err != nil {
		return "", err
	}
	parts := parseLine(gjson.ParseBytes(data))
	if loc.Part >= len(parts) {
		return "", fmt.Errorf("codex: no entry %d at offset %d of %s", loc.Part, loc.Offset, path)
	}
	return parts[loc.Part].text, nil
}

// idFromFilename returns the id in rollout-<time>-<id>.jsonl: the last five
// dash-separated groups of a UUID. Only a fallback for files with no header.
func idFromFilename(p string) string {
	base := strings.TrimSuffix(filepath.Base(p), ".jsonl")
	if f := strings.Split(base, "-"); len(f) >= 5 {
		return strings.Join(f[len(f)-5:], "-")
	}
	return base
}
