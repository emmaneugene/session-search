package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emmaneugene/session-search/internal/model"
)

// ReadOptions choose which part of a session to show.
type ReadOptions struct {
	At       int       // centre on this seq; -1 for none
	Around   time.Time // centre on the entry nearest this time
	Offset   int       // 1-based position among shown entries; 0 for none
	Context  int       // entries on each side of At or Around
	Max      int       // most entries to show
	MaxBytes int       // most bytes of output

	ToolResults bool
	Thinking    bool
	Injected    bool
	Abandoned   bool
	EntryBytes  int // cap per entry; tool results and calls get less
}

// Window is the part of a session that read returns.
type Window struct {
	Session model.Session `json:"session"`
	Total   int           `json:"total"` // entries shown with these options
	First   int           `json:"first"` // 1-based position of the first entry shown
	Entries []model.Entry `json:"entries"`
	// Truncated is true when MaxBytes cut the output short.
	Truncated bool `json:"truncated,omitempty"`
}

func (o ReadOptions) shows(e model.Entry) bool {
	switch {
	case e.Abandoned && !o.Abandoned:
		return false
	case e.Kind == model.KindToolResult:
		return o.ToolResults
	case e.Kind == model.KindThinking:
		return o.Thinking
	case e.Kind == model.KindInjected:
		return o.Injected
	}
	return true
}

// Read selects the window of entries to show.
func Read(s model.Session, all []model.Entry, o ReadOptions) Window {
	var shown []model.Entry
	for _, e := range all {
		if o.shows(e) {
			shown = append(shown, e)
		}
	}
	w := Window{Session: s, Total: len(shown), First: 1}
	length := max(o.Max, 1)
	start := 0
	center := -1
	switch {
	case o.At >= 0:
		center = sort.Search(len(shown), func(i int) bool { return shown[i].Seq >= o.At })
		center = min(center, len(shown)-1)
	case !o.Around.IsZero():
		bestGap := time.Duration(1 << 62)
		for i, e := range shown {
			if e.Time.IsZero() {
				continue
			}
			if gap := absDur(e.Time.Sub(o.Around)); gap < bestGap {
				center, bestGap = i, gap
			}
		}
	case o.Offset > 0:
		start = o.Offset - 1
	}
	if center >= 0 {
		// Centre the window on the entry, keeping it inside both the context
		// and the --max limit.
		length = min(length, 2*o.Context+1)
		start = center - (length-1)/2
		start = max(0, min(start, len(shown)-length))
	}
	if start >= len(shown) {
		w.First = start + 1
		return w // past the end: nothing to show
	}
	w.First = start + 1
	end := min(len(shown), start+length)

	// Budget the bytes of the rendered output: the session header first,
	// then each entry. JSON is the larger of the two formats, so measure it.
	budget := 0
	if o.MaxBytes > 0 {
		head, _ := json.Marshal(s)
		budget = o.MaxBytes - len(head) - 200
	}
	for _, e := range shown[start:end] {
		e.Text = clip(e.Text, entryCap(e.Kind, o.EntryBytes))
		if o.MaxBytes > 0 {
			size := entrySize(e)
			if size > budget {
				// Clip the entry to what's left, unless too little is left to
				// be useful; then stop.
				room := budget - (size - len(e.Text)) - 40
				if room < 200 {
					w.Truncated = true
					break
				}
				e.Text = clip(e.Text, room)
				w.Truncated = true
				w.Entries = append(w.Entries, e)
				break
			}
			budget -= size
		}
		w.Entries = append(w.Entries, e)
	}
	return w
}

func entrySize(e model.Entry) int {
	b, _ := json.Marshal(e)
	return len(b) + 40 // the Markdown heading line
}

func entryCap(k model.Kind, base int) int {
	if base <= 0 {
		base = 8000
	}
	switch k {
	case model.KindToolCall:
		return min(base, 600)
	case model.KindToolResult, model.KindInjected:
		return min(base, 4000)
	}
	return base
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("… [%d more bytes]", len(s)-cut)
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// Markdown renders a window for people and agents.
func (w Window) Markdown() string {
	var b strings.Builder
	s := w.Session
	fmt.Fprintf(&b, "# %s", s.Ref())
	if s.Title != "" {
		fmt.Fprintf(&b, " %s", s.Title)
	}
	b.WriteString("\n\n")
	field := func(name, value string) {
		if value != "" {
			fmt.Fprintf(&b, "- %s: %s\n", name, value)
		}
	}
	field("cwd", s.Cwd)
	field("file", s.File)
	field("started", fmtTime(s.Started))
	field("updated", fmtTime(s.Updated))
	field("parent", s.Parent)
	field("lacks", strings.Join(s.Lacks, ", "))
	if len(w.Entries) == 0 {
		switch {
		case w.First > w.Total && w.Total > 0:
			fmt.Fprintf(&b, "\nNo entry at position %d: %d entries are shown with these options.\n", w.First, w.Total)
		case w.Truncated:
			b.WriteString("\nNo entry fits in --max-bytes.\n")
		default:
			b.WriteString("\nNo entries to show with these options.\n")
		}
		return b.String()
	}
	last := w.First + len(w.Entries) - 1
	fmt.Fprintf(&b, "\nShowing entries %d to %d of %d.", w.First, last, w.Total)
	if last < w.Total {
		fmt.Fprintf(&b, " Next: --offset %d.", last+1)
	}
	if w.Truncated {
		b.WriteString(" Output was cut at the byte limit.")
	}
	b.WriteString("\n")
	for _, e := range w.Entries {
		fmt.Fprintf(&b, "\n## seq %d %s", e.Seq, e.Kind)
		if e.Tool != "" {
			fmt.Fprintf(&b, " %s", e.Tool)
		}
		if e.Abandoned {
			b.WriteString(" (abandoned branch)")
		}
		if !e.Time.IsZero() {
			fmt.Fprintf(&b, " · %s", e.Time.UTC().Format("2006-01-02 15:04:05Z"))
		}
		b.WriteString("\n\n")
		b.WriteString(strings.TrimSpace(e.Text))
		b.WriteString("\n")
	}
	return b.String()
}
