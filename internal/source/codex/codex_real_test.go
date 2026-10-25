package codex

import (
	"os"
	"testing"
	"time"

	"github.com/emmaneugene/session-search/internal/model"
	"github.com/emmaneugene/session-search/internal/source"
)

// Runs only with SESSION_SEARCH_REAL=1: parses every real Codex session.
func TestRealSessions(t *testing.T) {
	if os.Getenv("SESSION_SEARCH_REAL") != "1" {
		t.Skip("set SESSION_SEARCH_REAL=1 to read ~/.codex")
	}
	home, _ := os.UserHomeDir()
	env := source.Env{Home: home}
	start := time.Now()
	files, err := Source{}.Discover(env)
	if err != nil {
		t.Fatal(err)
	}
	var (
		kinds                = map[model.Kind]int{}
		failures, hidden     int
		slowest              string
		slowestTime          time.Duration
		textChecked, textBad int
	)
	for _, f := range files {
		t0 := time.Now()
		s, entries, err := Source{}.Parse(env, f.Path)
		if d := time.Since(t0); d > slowestTime {
			slowest, slowestTime = f.Path, d
		}
		if err != nil {
			failures++
			t.Errorf("%s: %v", f.Path, err)
			continue
		}
		if s.Hidden {
			hidden++
		}
		if s.ID == "" || s.Title == "" && !s.Hidden {
			t.Logf("odd session %s: id=%q title=%q", f.Path, s.ID, s.Title)
		}
		for i, e := range entries {
			kinds[e.Kind]++
			if i%7 == 0 { // a sample of entries
				textChecked++
				got, err := Source{}.Text(env, f.Path, e.Loc)
				if err != nil || got != e.Text {
					textBad++
					t.Errorf("Text mismatch in %s seq %d: %v", f.Path, e.Seq, err)
				}
			}
		}
	}
	t.Logf("files=%d parse errors=%d hidden=%d entries by kind=%v", len(files), failures, hidden, kinds)
	t.Logf("total %v, slowest %v (%s); Text checked %d, bad %d", time.Since(start), slowestTime, slowest, textChecked, textBad)
}
