package cursor

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/emmaneugene/session-search/internal/model"
	"github.com/emmaneugene/session-search/internal/source"
)

// TestRealSessions parses every Cursor chat on this machine. It only reads.
// Run with SESSION_SEARCH_REAL=1.
func TestRealSessions(t *testing.T) {
	if os.Getenv("SESSION_SEARCH_REAL") != "1" {
		t.Skip("set SESSION_SEARCH_REAL=1 to read the real Cursor folders")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	env := source.Env{Home: home}
	start := time.Now()
	files, err := Source{}.Discover(env)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("discover: %d files in %v", len(files), time.Since(start))

	db, _ := openDB(dbPath(env))
	if db != nil {
		defer db.Close()
	}
	var (
		parseErrs, hidden, subagents, rawCwd int
		kinds                                = map[model.Kind]int{}
		lacks                                = map[string]int{}
		slowest                              string
		slowestDur                           time.Duration
		dbChats, dbCalls, dbMatched          int
		dbToolBubbles                        int
		users, anchored                      int
		shellChecked, shellSame              int
		textChecked, textBad                 int
	)
	start = time.Now()
	for _, f := range files {
		t0 := time.Now()
		s, entries, err := Source{}.Parse(env, f.Path)
		if d := time.Since(t0); d > slowestDur {
			slowest, slowestDur = f.Path, d
		}
		if err != nil {
			parseErrs++
			t.Logf("parse %s: %v", f.Path, err)
			continue
		}
		if s.Hidden {
			hidden++
		}
		if s.Parent != "" {
			subagents++
		}
		if !strings.HasPrefix(s.Cwd, "/") {
			rawCwd++
		}
		for _, l := range s.Lacks {
			lacks[l]++
		}
		for _, e := range entries {
			kinds[e.Kind]++
		}

		// Alignment with the database, for chats it holds.
		if c, ok := loadChat(env, s.ID); ok && len(c.bubbles) > 0 {
			dbChats++
			for _, b := range c.bubbles {
				if b.tool != "" {
					dbToolBubbles++
				}
			}
			for i, e := range entries {
				switch e.Kind {
				case model.KindToolCall:
					dbCalls++
				case model.KindUser:
					users++
					for _, b := range c.bubbles {
						if b.user && strings.TrimSpace(b.text) == e.Text {
							anchored++
							break
						}
					}
				case model.KindToolResult:
					// A matched Shell call must run the command the bubble ran.
					call := entries[i-1]
					if call.Tool == "Shell" && db != nil {
						var v string
						if db.QueryRow(`SELECT value FROM cursorDiskKV WHERE key = ?`, e.Loc.Key).Scan(&v) == nil {
							shellChecked++
							params := gjson.Get(v, "toolFormerData.params").String()
							if gjson.Get(params, "command").String() == gjson.Get(call.Text, "command").String() {
								shellSame++
							} else if shellChecked-shellSame <= 3 {
								t.Logf("shell mismatch in %s seq %d: %.80q vs %.80q", s.ID, call.Seq, gjson.Get(call.Text, "command").String(), gjson.Get(params, "command").String())
							}
						}
					}
				}
			}
			items, recs, _ := readTranscript(f.Path)
			_, matched := merge(items, recs, c.bubbles)
			dbMatched += matched
		}

		// Text round trip on a sample: every 7th entry, and every tool result.
		for i, e := range entries {
			if i%7 != 0 && e.Kind != model.KindToolResult {
				continue
			}
			textChecked++
			got, err := Source{}.Text(env, f.Path, e.Loc)
			if err != nil || got != e.Text {
				textBad++
				if textBad <= 3 {
					t.Errorf("Text(%s seq %d) err=%v equal=%v", f.Path, e.Seq, err, got == e.Text)
				}
			}
		}
	}
	t.Logf("parse: %d files in %v, %d errors, slowest %v (%s)", len(files), time.Since(start), parseErrs, slowestDur, slowest)
	t.Logf("entries by kind: %v", kinds)
	t.Logf("sessions: %d hidden, %d with parent, %d with raw folder cwd, lacks %v", hidden, subagents, rawCwd, lacks)
	t.Logf("database: %d chats; %d of %d tool calls matched (%.1f%%); %d tool bubbles; user anchors %d of %d",
		dbChats, dbMatched, dbCalls, pct(dbMatched, dbCalls), dbToolBubbles, anchored, users)
	t.Logf("shell check: %d of %d matched results ran the same command (%.1f%%)", shellSame, shellChecked, pct(shellSame, shellChecked))
	t.Logf("text: %d checked, %d wrong", textChecked, textBad)
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}
