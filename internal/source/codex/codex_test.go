package codex

// Failure modes this reader must survive (from the real files and format notes):
//
//  1. The session id is session_meta payload.id, never the file name. A legacy
//     fork may carry a second session_meta for the parent; the first one wins.
//  2. event_msg repeats the user and assistant messages of response_item.
//     Indexing both duplicates everything. Older files have only the event
//     stream, so it is the fallback when no response_item message exists.
//  3. User turns carry injected context: AGENTS.md, <environment_context>,
//     <skill> bodies, developer messages, inter-agent messages. A user turn can
//     hold several blocks, and only some are injected.
//  4. Reasoning is often encrypted with an empty summary: no entry.
//     Encrypted compaction items and encrypted blocks: no entry.
//  5. Tool output is a string or a block array that can hold images.
//  6. Outputs name no tool: the name comes from the call with the same call_id.
//     Calls come in several shapes (function_call, custom_tool_call,
//     local_shell_call, web_search_call, tool_search_call).
//  7. compacted.replacement_history replays earlier messages; only a non-empty
//     message is a summary.
//  8. Forks replay the parent's history in a burst at the start of the file.
//  9. Review threads (model codex-auto-review in turn_context or in thread
//     settings) and threads with no user or assistant text are hidden.
// 10. Titles come from session_index.jsonl (last line per id wins; an empty
//     name falls back to the first user text). A title edit does not touch the
//     session file, so the index must be part of every fingerprint.
// 11. A broken line (truncated write) is skipped, not fatal.
// 12. Text must round-trip from Loc for every entry, including entries that
//     share one line.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emmaneugene/session-search/internal/source"
	"github.com/emmaneugene/session-search/internal/source/sourcetest"
)

func TestDiscover(t *testing.T) {
	env := sourcetest.Env(t)
	files, err := Source{}.Discover(env)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 6 {
		t.Fatalf("got %d files, want 6: %v", len(files), files)
	}
	for _, f := range files {
		if f.Fingerprint == "" {
			t.Errorf("%s has no fingerprint", f.Path)
		}
	}
	sourcetest.Find(t, files, "sessions/2026/10/01/rollout-2026-10-01T00-00-00-019fa000-ffff-7000-8000-000000000000.jsonl")
	sourcetest.Find(t, files, "archived_sessions/rollout-2026-06-01T10-00-00-019f0000-0000-7000-8000-00000000bbbb.jsonl")
}

func TestFingerprintChangesWithTitleIndex(t *testing.T) {
	home := t.TempDir()
	copyTree(t, sourcetest.Env(t).Home, home)
	env := source.Env{Home: home}
	before, _ := Source{}.Discover(env)
	index := filepath.Join(home, ".codex", "session_index.jsonl")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(index, later, later); err != nil {
		t.Fatal(err)
	}
	after, _ := Source{}.Discover(env)
	if len(before) != len(after) || len(before) == 0 {
		t.Fatalf("file counts %d, %d", len(before), len(after))
	}
	prev := map[string]string{}
	for _, f := range before {
		prev[f.Path] = f.Fingerprint
	}
	for _, f := range after {
		if prev[f.Path] == f.Fingerprint {
			t.Errorf("%s fingerprint did not change with the index", f.Path)
		}
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(filepath.Join(from, ".codex"), func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestParseMainSession(t *testing.T) {
	env := sourcetest.Env(t)
	files, _ := Source{}.Discover(env)
	f := sourcetest.Find(t, files, "019fa000-ffff-7000-8000-000000000000.jsonl")
	s, entries, err := Source{}.Parse(env, f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Agent != "codex" || s.ID != "019fa000-0000-7000-8000-00000000aaaa" || s.Cwd != "/Users/me/proj" || s.Title != "Login fix" {
		t.Errorf("session = %+v", s)
	}
	if s.Parent != "" || len(s.Lacks) != 0 || s.Hidden || s.File != f.Path {
		t.Errorf("parent/lacks/hidden/file = %q %v %v %q", s.Parent, s.Lacks, s.Hidden, s.File)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); !s.Started.Equal(want) {
		t.Errorf("started = %v, want %v", s.Started, want)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 16, 0, time.UTC); !s.Updated.Equal(want) {
		t.Errorf("updated = %v, want %v", s.Updated, want)
	}
	sourcetest.Equal(t, sourcetest.Lines(entries), []string{
		"0 injected <permissions instructions>\nsandbox",
		"1 injected # AGENTS.md instructions for /Users/me/proj\n\n<INSTRUCTIONS>\nBe nice\n</INSTRUCTIONS>",
		"2 injected <environment_context>\n  <cwd>/Users/me/proj</cwd>\n</environment_context>",
		"3 user Fix the login bug",
		"4 thinking **Looking at auth**\n\nThen tests",
		"5 assistant Let me look",
		`6 tool_call[exec_command] {"cmd":"ls src","workdir":"/Users/me/proj"}`,
		"7 tool_result[exec_command] login.ts\nauth.ts",
		"8 tool_call[apply_patch] *** Begin Patch\n*** End Patch",
		"9 tool_result[apply_patch] Script completed\nDone",
		"10 tool_call[local_shell] bash -lc git status",
		"11 tool_result[local_shell] nothing to commit",
		"12 tool_call[web_search] codex session format",
		`13 tool_call[tool_search] {"query":"docs search"}`,
		"14 tool_result[tool_search] mcp__docs\nsearch_docs\nfetch_doc",
		"15 injected Message Type: NEW_TASK\n",
		"16 summary Summary of earlier work",
		"17 user [$review](/Users/me/.codex/skills/review/SKILL.md) \n",
		"18 injected <skill>\n<name>review</name>\nbody\n</skill>",
		"19 user What is in this picture?",
		"20 assistant A cat",
	})
	if want := time.Date(2026, 10, 1, 0, 0, 2, 0, time.UTC); !entries[3].Time.Equal(want) {
		t.Errorf("entry 3 time = %v, want %v", entries[3].Time, want)
	}
	sourcetest.CheckText(t, Source{}, env, f.Path, entries)
}

func TestParseEventOnlyForkWithBrokenLine(t *testing.T) {
	env := sourcetest.Env(t)
	files, _ := Source{}.Discover(env)
	f := sourcetest.Find(t, files, "00000000bbbb.jsonl")
	s, entries, err := Source{}.Parse(env, f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "019f0000-0000-7000-8000-00000000bbbb" || s.Parent != "codex:019f0000-0000-7000-8000-00000000cccc" || s.Title != "Old style question" || s.Hidden {
		t.Errorf("session = %+v", s)
	}
	sourcetest.Equal(t, sourcetest.Lines(entries), []string{
		"0 user Old style question",
		"1 thinking thinking it over",
		`2 tool_call[shell] {"command":["ls"]}`,
		`3 tool_result[shell] {"output":"a.txt","metadata":{"exit_code":0}}`,
		"4 assistant Old style answer",
	})
	sourcetest.CheckText(t, Source{}, env, f.Path, entries)
}

func TestHiddenSessions(t *testing.T) {
	env := sourcetest.Env(t)
	files, _ := Source{}.Discover(env)
	for suffix, title := range map[string]string{
		"00000000dddd.jsonl": "Review this command: rm -rf x", // empty index name falls back to the user text
		"00000000eeee.jsonl": "Check this",
		"00000000ffff.jsonl": "",
	} {
		f := sourcetest.Find(t, files, suffix)
		s, entries, err := Source{}.Parse(env, f.Path)
		if err != nil {
			t.Fatal(err)
		}
		if !s.Hidden || s.Title != title {
			t.Errorf("%s: hidden=%v title=%q", suffix, s.Hidden, s.Title)
		}
		sourcetest.CheckText(t, Source{}, env, f.Path, entries)
	}
}

func TestForkReplayIsDropped(t *testing.T) {
	env := sourcetest.Env(t)
	files, _ := Source{}.Discover(env)
	f := sourcetest.Find(t, files, "019f1111-0000-7000-8000-00000000aaaa.jsonl")
	s, entries, err := Source{}.Parse(env, f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Parent != "codex:019fa000-0000-7000-8000-00000000aaaa" || s.Title != "Fork question" {
		t.Errorf("session = %+v", s)
	}
	sourcetest.Equal(t, sourcetest.Lines(entries), []string{
		"0 user Fork question",
		"1 assistant Fork answer",
	})
	sourcetest.CheckText(t, Source{}, env, f.Path, entries)
}
