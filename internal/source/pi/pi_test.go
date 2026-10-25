package pi

import (
	"testing"
	"time"

	"github.com/emmaneugene/session-search/internal/source/sourcetest"
)

func TestDiscoverFindsNestedSessionsOnly(t *testing.T) {
	env := sourcetest.Env(t)
	files, err := Source{}.Discover(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("got %d files, want 3 (main, fork, subagent): %v", len(files), files)
	}
	for _, f := range files {
		if f.Fingerprint == "" {
			t.Errorf("%s has no fingerprint", f.Path)
		}
	}
	sourcetest.Find(t, files, "subagents/sess-main-0001/2026-10-01T01-00-00-000Z_sess-sub-0002.jsonl")
}

func TestParseMainSession(t *testing.T) {
	env := sourcetest.Env(t)
	files, _ := Source{}.Discover(env)
	f := sourcetest.Find(t, files, "_sess-main-0001.jsonl")
	s, entries, err := Source{}.Parse(env, f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Agent != "pi" || s.ID != "sess-main-0001" || s.Cwd != "/Users/me/proj" || s.Title != "Login fix" {
		t.Errorf("session = %+v", s)
	}
	if s.Parent != "" || len(s.Lacks) != 0 || s.Hidden {
		t.Errorf("session parent/lacks/hidden = %q %v %v", s.Parent, s.Lacks, s.Hidden)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); !s.Started.Equal(want) {
		t.Errorf("started = %v, want %v", s.Started, want)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 13, 0, time.UTC); !s.Updated.Equal(want) {
		t.Errorf("updated = %v, want %v", s.Updated, want)
	}
	sourcetest.Equal(t, sourcetest.Lines(entries), []string{
		"0 user Fix the login bug",
		"1 thinking Considering the auth flow",
		"2 assistant Let me look",
		`3 tool_call[bash] {"command":"ls src"}`,
		"4 tool_result[bash] login.ts",
		"5 user!abandoned Try approach A",
		"6 assistant!abandoned Approach A failed",
		"7 summary Tried approach A",
		"8 user Try approach B",
		"9 injected Extension note",
		"10 tool_call[bash] git status",
		"11 tool_result[bash] nothing to commit",
		"12 summary Summary of work",
		"13 assistant Approach B worked",
	})
	if want := time.Date(2026, 10, 1, 0, 0, 2, 0, time.UTC); !entries[0].Time.Equal(want) {
		t.Errorf("entry 0 time = %v, want %v", entries[0].Time, want)
	}
	sourcetest.CheckText(t, Source{}, env, f.Path, entries)
}

func TestParseSubagentAndForkParents(t *testing.T) {
	env := sourcetest.Env(t)
	files, _ := Source{}.Discover(env)

	sub := sourcetest.Find(t, files, "_sess-sub-0002.jsonl")
	s, entries, err := Source{}.Parse(env, sub.Path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "sess-sub-0002" || s.Parent != "pi:sess-main-0001" {
		t.Errorf("subagent session = %+v", s)
	}
	if want := "Review the login change and report problems with session cookies in a long sente…"; s.Title != want {
		t.Errorf("title = %q, want %q", s.Title, want)
	}
	sourcetest.CheckText(t, Source{}, env, sub.Path, entries)

	fork := sourcetest.Find(t, files, "_sess-fork-0003.jsonl")
	s, _, err = Source{}.Parse(env, fork.Path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Parent != "pi:sess-main-0001" {
		t.Errorf("fork parent = %q", s.Parent)
	}
}
