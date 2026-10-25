package claude

// Failure modes this reader must survive (from the real files):
//
//  1. Every content block of an assistant message is its own record, and
//     parallel tool calls give each result a different parent, so results
//     of earlier calls sit off the leaf path without being abandoned.
//  2. Rewinds fork the parentUuid tree; entries off the leaf path are abandoned.
//  3. A compaction boundary has parentUuid null and the real parent in
//     logicalParentUuid; following only parentUuid would abandon everything
//     before the compaction.
//  4. Records without uuid (file-history-snapshot, ai-title, last-prompt) can
//     be the last line, so the leaf is the last record with a uuid.
//  5. tool_result sits in a user record, content is a string or blocks that
//     may hold images or tool_reference; the tool name comes from the
//     matching tool_use id, which can be on an earlier line.
//  6. User text that is not human: isMeta skill bodies, system-reminder blocks,
//     command wrappers, local-command output, task notifications.
//  7. A slash command wrapper with arguments carries the human's request.
//  8. isCompactSummary user records are summaries.
//  9. Empty thinking blocks (signature only) and images are skipped.
// 10. Titles: custom-title beats ai-title beats first user text; subagents use
//     their meta.json description.
// 11. Subagent files: sessionId is the parent's id, the file name is the id.
// 12. The file name, not sessionId, is the session id (resumed sessions copy
//     records with another sessionId).
// 13. Truncated or invalid JSON lines are skipped.
// 14. Sessions without a human message are Hidden.
// 15. Text(loc) must return the same text from one line, including blocks
//     that share a line (Part).

import (
	"strings"
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
	if len(files) != 4 {
		t.Fatalf("got %d files, want 4: %v", len(files), files)
	}
	for _, f := range files {
		if f.Fingerprint == "" {
			t.Errorf("%s has no fingerprint", f.Path)
		}
	}
	sourcetest.Find(t, files, "sess-main-0001/subagents/agent-a1b2.jsonl")
	if files, err := (Source{}).Discover(sourceEnv(t.TempDir())); err != nil || len(files) != 0 {
		t.Errorf("missing folder: %v %v", files, err)
	}
}

func TestParseMainSession(t *testing.T) {
	env := sourcetest.Env(t)
	files, _ := Source{}.Discover(env)
	f := sourcetest.Find(t, files, "/sess-main-0001.jsonl")
	s, entries, err := Source{}.Parse(env, f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Agent != "claude" || s.ID != "sess-main-0001" || s.Cwd != "/Users/me/proj" || s.Title != "Login fix" {
		t.Errorf("session = %+v", s)
	}
	if s.Parent != "" || len(s.Lacks) != 0 || s.Hidden {
		t.Errorf("parent/lacks/hidden = %q %v %v", s.Parent, s.Lacks, s.Hidden)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 1, 0, time.UTC); !s.Started.Equal(want) {
		t.Errorf("started = %v, want %v", s.Started, want)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 20, 0, time.UTC); !s.Updated.Equal(want) {
		t.Errorf("updated = %v, want %v", s.Updated, want)
	}
	sourcetest.Equal(t, sourcetest.Lines(entries), []string{
		"0 user Fix the login bug",
		"1 thinking Considering auth",
		"2 assistant Let me look",
		`3 tool_call[Bash] {"command":"ls src"}`,
		`4 tool_call[Read] {"file_path":"/x/login.ts"}`,
		"5 tool_result[Bash] login.ts",
		"6 tool_result[Read] file body",
		"7 user!abandoned Try approach A",
		"8 assistant!abandoned Approach A failed",
		"9 injected <system-reminder>Be brief</system-reminder>",
		"10 user Try approach B",
		"11 injected Base directory for this skill: /s\n\nSkill body",
		"12 assistant Approach B worked",
		"13 injected <command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args></command-args>",
		"14 injected <local-command-stdout>Set model to X</local-command-stdout>",
		"15 summary This session is being continued from a previous conversation.\n\nSummary:\nTried A then B.",
		"16 user /review check the diff",
		"17 assistant Done",
	})
	if want := time.Date(2026, 10, 1, 0, 0, 1, 0, time.UTC); !entries[0].Time.Equal(want) {
		t.Errorf("entry 0 time = %v, want %v", entries[0].Time, want)
	}
	sourcetest.CheckText(t, Source{}, env, f.Path, entries)
}

func TestParseSubagent(t *testing.T) {
	env := sourcetest.Env(t)
	files, _ := Source{}.Discover(env)
	f := sourcetest.Find(t, files, "agent-a1b2.jsonl")
	s, entries, err := Source{}.Parse(env, f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "agent-a1b2" || s.Parent != "claude:sess-main-0001" || s.Title != "Review the docs" || s.Hidden {
		t.Errorf("session = %+v", s)
	}
	sourcetest.Equal(t, sourcetest.Lines(entries), []string{"0 user Check the docs", "1 assistant Docs look fine"})
	sourcetest.CheckText(t, Source{}, env, f.Path, entries)
}

func TestTitleFallbackAndBadLines(t *testing.T) {
	env := sourcetest.Env(t)
	files, _ := Source{}.Discover(env)
	f := sourcetest.Find(t, files, "sess-plain-0002.jsonl")
	s, entries, err := Source{}.Parse(env, f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.Title, "Plain question about xxx") || len([]rune(s.Title)) > 81 {
		t.Errorf("title = %q", s.Title)
	}
	if len(entries) != 2 {
		t.Errorf("entries = %v", sourcetest.Lines(entries))
	}
}

func TestSessionWithoutHumanIsHidden(t *testing.T) {
	env := sourcetest.Env(t)
	files, _ := Source{}.Discover(env)
	f := sourcetest.Find(t, files, "sess-empty-0003.jsonl")
	s, entries, err := Source{}.Parse(env, f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Hidden || s.ID != "sess-empty-0003" {
		t.Errorf("session = %+v", s)
	}
	sourcetest.Equal(t, sourcetest.Lines(entries), []string{"0 injected <local-command-stdout>Bye</local-command-stdout>"})
}

func sourceEnv(home string) source.Env { return source.Env{Home: home} }
