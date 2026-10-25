package claude

import (
	"os"
	"testing"
	"time"

	"github.com/emmaneugene/session-search/internal/model"
	"github.com/emmaneugene/session-search/internal/source"
)

// TestRealSessions parses every real session. Run with SESSION_SEARCH_REAL=1.
func TestRealSessions(t *testing.T) {
	if os.Getenv("SESSION_SEARCH_REAL") != "1" {
		t.Skip("set SESSION_SEARCH_REAL=1 to read ~/.claude")
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
	kinds := map[model.Kind]int{}
	var errs, hidden, subs, abandoned, untitled int
	var slowest time.Duration
	var slowFile string
	for _, f := range files {
		t0 := time.Now()
		s, entries, err := Source{}.Parse(env, f.Path)
		if d := time.Since(t0); d > slowest {
			slowest, slowFile = d, f.Path
		}
		if err != nil {
			errs++
			t.Errorf("%s: %v", f.Path, err)
			continue
		}
		if s.Hidden {
			hidden++
		}
		if s.Parent != "" {
			subs++
		}
		if s.Title == "" {
			untitled++
		}
		for i, e := range entries {
			kinds[e.Kind]++
			if e.Abandoned {
				abandoned++
			}
			if i%7 == 0 {
				got, err := Source{}.Text(env, f.Path, e.Loc)
				if err != nil || got != e.Text {
					t.Errorf("%s seq %d: Text mismatch (%v)", f.Path, e.Seq, err)
				}
			}
		}
	}
	t.Logf("files=%d errors=%d hidden=%d subagents=%d untitled=%d abandoned=%d kinds=%v total=%v slowest=%v (%s)",
		len(files), errs, hidden, subs, untitled, abandoned, kinds, time.Since(start), slowest, slowFile)
}
