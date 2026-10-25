// Package model holds the normalized session and entry types that every
// source reader produces and that the index, search and read commands use.
package model

import "time"

// Kind is what an entry holds.
type Kind string

const (
	KindUser       Kind = "user"
	KindAssistant  Kind = "assistant"
	KindThinking   Kind = "thinking"
	KindToolCall   Kind = "tool_call"
	KindToolResult Kind = "tool_result"
	KindSummary    Kind = "summary"  // compaction or branch summaries
	KindInjected   Kind = "injected" // AGENTS.md, skill bodies, environment blocks, extension messages
)

// Lack names a kind of information that a session's source doesn't keep.
const (
	LackToolResults = "tool_results"
	LackTimestamps  = "timestamps"
)

// Session is one conversation: one transcript file, plus whatever extra
// stores the source merges in.
type Session struct {
	Agent   string    `json:"agent"`
	ID      string    `json:"id"`
	File    string    `json:"file"`
	Cwd     string    `json:"cwd,omitempty"`
	Title   string    `json:"title,omitempty"`
	Started time.Time `json:"started,omitzero"`
	Updated time.Time `json:"updated,omitzero"`
	// Parent is the reference ("agent:id") of the session that spawned or
	// forked this one, if any.
	Parent string   `json:"parent,omitempty"`
	Lacks  []string `json:"lacks,omitempty"`
	// Hidden sessions (for example Codex automatic review threads) are left
	// out of search and list results unless the caller asks for them.
	Hidden bool `json:"hidden,omitempty"`
}

// Ref is the "agent:id" reference for a session.
func (s Session) Ref() string { return s.Agent + ":" + s.ID }

// Loc tells the source reader where an entry's text lives, so a snippet can
// be rebuilt without parsing the whole file. Readers decide what the fields
// mean. JSONL readers use Offset and Length for the line and Part for the
// entry's position among the entries parsed from that line. Key is for
// stores that aren't line files, such as a database row key.
type Loc struct {
	Offset int64  `json:"offset,omitempty"`
	Length int    `json:"length,omitempty"`
	Part   int    `json:"part,omitempty"`
	Key    string `json:"key,omitempty"`
}

// Entry is one searchable or readable piece of a session.
type Entry struct {
	// Seq is the entry's 0-based position in the session, in file order.
	Seq  int       `json:"seq"`
	Time time.Time `json:"time,omitzero"`
	Kind Kind      `json:"kind"`
	Text string    `json:"text"`
	// Tool is the tool name for tool_call and tool_result entries.
	Tool string `json:"tool,omitempty"`
	// Abandoned is true for entries on a branch the user left (Pi trees,
	// Claude Code rewinds).
	Abandoned bool `json:"abandoned,omitempty"`
	Loc       Loc  `json:"-"`
}
