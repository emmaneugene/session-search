package cursor

import (
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/emmaneugene/session-search/internal/model"
)

// part is one entry parsed from a transcript line.
type part struct {
	kind model.Kind
	text string
	tool string
}

// record is what one transcript line holds. Lines are
// {"role":..., "message":{"content":[blocks]}} or {"type":"turn_ended"}.
type record struct {
	role  string
	parts []part
	// when is the first <timestamp> tag of a user record, minute precision.
	when time.Time
	// boundary is set on the user record that opens a side chat; the lines
	// before it repeat the parent chat's history.
	boundary bool
}

func parseLine(data []byte) (record, bool) {
	if !gjson.ValidBytes(data) {
		return record{}, false
	}
	v := gjson.ParseBytes(data)
	r := record{role: v.Get("role").String()}
	content := v.Get("message.content")
	switch r.role {
	case "user":
		r.parts, r.when, r.boundary = splitUser(contentText(content))
	case "assistant":
		if content.Type == gjson.String {
			if s := content.String(); s != "" {
				r.parts = append(r.parts, part{kind: model.KindAssistant, text: s})
			}
			break
		}
		content.ForEach(func(_, c gjson.Result) bool {
			switch c.Get("type").String() {
			case "text":
				if s := c.Get("text").String(); s != "" {
					r.parts = append(r.parts, part{kind: model.KindAssistant, text: s})
				}
			case "tool_use":
				r.parts = append(r.parts, part{kind: model.KindToolCall, tool: c.Get("name").String(), text: compactJSON(c.Get("input"))})
			}
			return true
		})
	}
	return r, true
}

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

const boundaryTag = "<side_chat_boundary>"

// splitUser splits a user record's text. Cursor wraps what the person typed
// in <user_query> and puts a <timestamp> tag before it; everything else
// (<user_info>, attached skills, rules, system reminders) was added by
// Cursor and becomes one injected entry per run of such text.
func splitUser(text string) (parts []part, when time.Time, boundary bool) {
	var injected []string
	flush := func() {
		s := strings.TrimSpace(strings.Join(injected, "\n"))
		injected = nil
		if s == "" {
			return
		}
		if i := strings.Index(s, boundaryTag); i >= 0 {
			// Anything before the boundary is inherited from the parent.
			parts, boundary, s = nil, true, s[i:]
		}
		parts = append(parts, part{kind: model.KindInjected, text: s})
	}
	for text != "" {
		ts, q := strings.Index(text, "<timestamp>"), strings.Index(text, "<user_query>")
		i, tag := ts, "timestamp"
		if i < 0 || (q >= 0 && q < i) {
			i, tag = q, "user_query"
		}
		if i < 0 {
			injected = append(injected, dropImages(text))
			break
		}
		injected = append(injected, dropImages(text[:i]))
		body := text[i+len(tag)+2:]
		end := strings.Index(body, "</"+tag+">")
		if end < 0 {
			end = len(body)
			text = ""
		} else {
			text = body[end+len(tag)+3:]
		}
		body = body[:end]
		if tag == "timestamp" {
			if t := parseTag(body); when.IsZero() {
				when = t
			}
			continue
		}
		flush()
		if s := strings.TrimSpace(body); s != "" {
			parts = append(parts, part{kind: model.KindUser, text: s})
		}
	}
	flush()
	return parts, when, boundary
}

// dropImages removes the "[Image]" lines that stand in for pasted images.
func dropImages(s string) string {
	if !strings.Contains(s, "[Image]") {
		return s
	}
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.TrimSpace(l) != "[Image]" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// parseTag reads "Friday, Oct 23, 2026, 11:01 AM (UTC+8)". The offset can
// have minutes ("UTC+5:30") or be missing, which means UTC.
func parseTag(s string) time.Time {
	s = strings.TrimSpace(s)
	if _, rest, ok := strings.Cut(s, ", "); ok {
		s = rest // weekday
	}
	loc := time.UTC
	if i := strings.Index(s, " ("); i >= 0 {
		off := strings.TrimSuffix(strings.TrimPrefix(s[i+2:], "UTC"), ")")
		s = s[:i]
		if off != "" {
			sign := 1
			if off[0] == '-' {
				sign = -1
			}
			h, m, _ := strings.Cut(off[1:], ":")
			hh, err1 := strconv.Atoi(h)
			mm := 0
			if m != "" {
				mm, _ = strconv.Atoi(m)
			}
			if err1 != nil {
				return time.Time{}
			}
			loc = time.FixedZone("", sign*(hh*3600+mm*60))
		}
	}
	t, err := time.ParseInLocation("Jan 2, 2006, 3:04 PM", s, loc)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
