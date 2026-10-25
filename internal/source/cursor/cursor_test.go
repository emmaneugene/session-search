package cursor

// Failure modes this reader must handle, from the real files on this
// machine and the format notes in docs/design.md:
//
//  1. Transcripts live at agent-transcripts/<id>/<id>.jsonl, subagents and
//     side chats at <id>/subagents/<sub>.jsonl, and an old flat layout at
//     agent-transcripts/<id>.jsonl. Other project folders (mcps/, canvases/,
//     agent-tools/, terminals/) hold .jsonl files that aren't chats.
//  2. The same chat id can sit in two project folders (a chat moved between
//     windows, or a chat copied as a side chat under another chat): keep one
//     session per id, the most recently written file.
//  3. A user record mixes the real prompt (<user_query>) with a
//     <timestamp> tag, context blocks (<user_info>, skills, rules) and
//     "[Image]" placeholders. Some user records have no <user_query> at all.
//  4. A side chat's transcript starts with the parent's whole history, then
//     a <side_chat_boundary> block; only what follows belongs to the side chat.
//  5. turn_ended lines, cut-off JSON lines, and string content instead of a
//     block array.
//  6. Transcript tool calls have no ids. The database names tools
//     differently (Shell is run_terminal_command_v2), orders parallel calls
//     differently, has tool bubbles the transcript lacks, and lacks some
//     the transcript has (which must not shift later results onto the wrong
//     call). Cursor also rewrites some arguments (git commit gains a
//     --trailer), so arguments can't be required to match.
//  7. Two database layouts: fullConversationHeadersOnly in composerData
//     (schema 16 and 17) and headerLog with composerConvHeaders:<id>:<n>
//     chunks (schema 18). A header can name a bubble row that is missing.
//  8. A tool result is a JSON string whose text field depends on the tool
//     (output, contents, markdown, error...). Some results carry only
//     metadata (content ids, line counts) and must not become entries.
//  9. No database, or a chat missing from it: tool results are lacking, and
//     times come from the <timestamp> tags; with no tags either, timestamps
//     are lacking.
// 10. The database path has a space, and Cursor holds it open in WAL mode.
// 11. Cwd: composerData workspaceIdentifier.uri.fsPath when present (a
//     configPath is a .code-workspace file, not a folder); else the
//     command-line meta.json; else the folder name decoded against the real
//     file system, where dashes are ambiguous and dots are dropped; else the
//     raw folder name.
// 12. Title: composerData name, else the first user query.
// 13. Parent: the subagents folder, or composerData subagentInfo.parentComposerId.
// 14. agent-<uuid> transcripts are Cursor SDK runs (Pi's Cursor provider);
//     the Pi session file holds the same conversation, so they aren't Cursor
//     sessions at all: Discover skips them.
// 15. The fingerprint must change when the chat's database rows change, even
//     when the transcript file doesn't.
// 16. <timestamp> tags use offsets like UTC+8 and UTC+5:30.

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/emmaneugene/session-search/internal/model"
	"github.com/emmaneugene/session-search/internal/source"
	"github.com/emmaneugene/session-search/internal/source/sourcetest"
)

func ms(t time.Time) int64 { return t.UnixMilli() }

func at(h, m, s int) time.Time { return time.Date(2026, 10, 23, h, m, s, 0, time.UTC) }

// home copies the shared fixture's Cursor folders into a temp home, so the
// test can add a database and set modified times without touching testdata.
func home(t *testing.T, withDB bool) source.Env {
	t.Helper()
	src := sourcetest.Env(t).Home
	dst := t.TempDir()
	for _, d := range []string{".cursor", ".config/cursor"} {
		if err := os.MkdirAll(filepath.Join(dst, filepath.Dir(d)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.CopyFS(filepath.Join(dst, d), os.DirFS(filepath.Join(src, d))); err != nil {
			t.Fatal(err)
		}
	}
	old := filepath.Join(dst, ".cursor/projects/Users-me-old/agent-transcripts/chat-ide-0001/chat-ide-0001.jsonl")
	if err := os.Chtimes(old, at(1, 0, 0), at(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if withDB {
		writeDB(t, dbPath(source.Env{Home: dst}), fixtureRows())
	}
	return source.Env{Home: dst}
}

func writeDB(t *testing.T, path string, rows map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS cursorDiskKV (key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	for k, v := range rows {
		// Cursor stores the JSON as TEXT in a BLOB column.
		var val any
		if v != nil {
			b, _ := json.Marshal(v)
			val = string(b)
		}
		if _, err := db.Exec(`INSERT INTO cursorDiskKV VALUES (?, ?)`, k, val); err != nil {
			t.Fatal(err)
		}
	}
}

type obj = map[string]any

func tool(name string, params obj, result any, created, done time.Time) obj {
	p, _ := json.Marshal(params)
	r := ""
	if result != nil {
		b, _ := json.Marshal(result)
		r = string(b)
	}
	return obj{"type": 2, "createdAt": created.Format(time.RFC3339Nano), "completedAtMs": ms(done),
		"toolFormerData": obj{"name": name, "status": "completed", "params": string(p), "result": r}}
}

func fixtureRows() map[string]any {
	ide := "chat-ide-0001"
	b := func(id string) string { return "bubbleId:" + ide + ":" + id }
	headers := []obj{}
	for _, id := range []string{"u1", "th", "a1", "t1", "t3", "t2", "tx", "gone", "a2", "u2", "t4", "t5", "u3", "a3"} {
		headers = append(headers, obj{"bubbleId": id, "type": 2})
	}
	side := "chat-side-0003"
	return map[string]any{
		"composerData:" + ide: obj{"_v": 17, "composerId": ide, "name": "Fix login",
			"createdAt": ms(at(3, 0, 59)), "lastUpdatedAt": ms(at(3, 10, 0)),
			"fullConversationHeadersOnly": headers,
			"workspaceIdentifier":         obj{"id": "w1", "uri": obj{"fsPath": "/Users/me/proj"}}},
		b("u1"):   obj{"type": 1, "text": "Fix the login bug", "createdAt": "2026-10-23T03:01:02Z"},
		b("th"):   obj{"type": 2, "thinking": obj{"text": "Considering"}, "createdAt": "2026-10-23T03:01:03Z"},
		b("a1"):   obj{"type": 2, "text": "Let me look", "createdAt": "2026-10-23T03:01:04Z"},
		b("t1"):   tool("run_terminal_command_v2", obj{"command": "ls src -la"}, obj{"output": "login.ts", "rejected": false}, at(3, 1, 5), at(3, 1, 6)),
		b("t3"):   tool("ripgrep_raw_search", obj{"pattern": "login"}, obj{"error": obj{"error": "Timed out after 25s"}}, at(3, 1, 7), at(3, 1, 9)),
		b("t2"):   tool("read_file_v2", obj{"targetFile": "a.ts"}, obj{"contents": "export const a = 1", "totalLinesInFile": 1}, at(3, 1, 7), at(3, 1, 8)),
		b("tx"):   tool("todo_write", nil, obj{"success": true}, at(3, 1, 10), at(3, 1, 10)),
		b("a2"):   obj{"type": 2, "text": "Found it", "createdAt": "2026-10-23T03:01:12Z"},
		b("u2"):   obj{"type": 1, "text": "now commit", "createdAt": "2026-10-23T03:05:10Z"},
		b("t4"):   tool("edit_file_v2", obj{"relativeWorkspacePath": "a.ts"}, obj{"beforeContentId": "x", "afterContentId": "y"}, at(3, 5, 12), at(3, 5, 13)),
		b("t5"):   tool("run_terminal_command_v2", obj{"command": "git commit"}, obj{"output": "[main abc] fix", "exitCode": 0}, at(3, 5, 14), at(3, 5, 16)),
		b("u3"):   obj{"type": 1, "text": "Briefly inform the user about the task result", "createdAt": "2026-10-23T03:09:30Z"},
		b("a3"):   obj{"type": 2, "text": "Done", "createdAt": "2026-10-23T03:09:40Z"},
		b("null"): nil,

		"composerData:" + side: obj{"_v": 18, "composerId": side, "name": "",
			"createdAt": ms(at(3, 20, 0)), "lastUpdatedAt": ms(at(3, 21, 0)),
			"fullConversationHeadersOnly": []obj{},
			"headerLog":                   obj{"chunkSize": 256, "chunkCount": 1, "count": 3},
			"subagentInfo":                obj{"parentComposerId": ide, "subagentTypeName": "side-chat"},
			"workspaceIdentifier":         obj{"id": "w2", "configPath": obj{"fsPath": "/Users/me/Library/x.code-workspace"}}},
		"composerConvHeaders:" + side + ":000000": []obj{{"bubbleId": "b1", "type": 1}, {"bubbleId": "b1/s:0", "type": 2}, {"bubbleId": "b1/t:toolu_1", "type": 2}},
		"bubbleId:" + side + ":b1":                obj{"type": 1, "text": "why that file?", "createdAt": "2026-10-23T03:20:05Z"},
		"bubbleId:" + side + ":b1/s:0":            obj{"type": 2, "text": "Because", "createdAt": "2026-10-23T03:20:07Z"},
		"bubbleId:" + side + ":b1/t:toolu_1":      tool("glob_file_search", obj{"globPattern": "*.ts"}, obj{"directories": []obj{{"absPath": "/x", "files": []obj{{"relPath": "a.ts"}}}}}, at(3, 20, 8), at(3, 20, 9)),
	}
}

func parse(t *testing.T, env source.Env, suffix string) (model.Session, []model.Entry) {
	t.Helper()
	files, err := Source{}.Discover(env)
	if err != nil {
		t.Fatal(err)
	}
	f := sourcetest.Find(t, files, suffix)
	s, entries, err := Source{}.Parse(env, f.Path)
	if err != nil {
		t.Fatal(err)
	}
	sourcetest.CheckText(t, Source{}, env, f.Path, entries)
	return s, entries
}

func TestDiscoverOneFilePerChat(t *testing.T) {
	env := home(t, true)
	files, err := Source{}.Discover(env)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		if f.Fingerprint == "" {
			t.Errorf("%s has no fingerprint", f.Path)
		}
		got = append(got, strings.TrimPrefix(f.Path, env.Home+"/.cursor/projects/"))
	}
	want := map[string]bool{
		"Users-me-proj/agent-transcripts/chat-ide-0001/chat-ide-0001.jsonl":            true,
		"Users-me-proj/agent-transcripts/chat-ide-0001/subagents/chat-side-0003.jsonl": true,
		"Users-me-proj/agent-transcripts/chat-flat-0004.jsonl":                         true,
		"Users-me-old/agent-transcripts/chat-cli-0002/chat-cli-0002.jsonl":             true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d files, want %d: %v", len(got), len(want), got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected file %s", g)
		}
	}
}

func TestDiscoverWithoutCursor(t *testing.T) {
	files, err := Source{}.Discover(source.Env{Home: t.TempDir()})
	if err != nil || len(files) != 0 {
		t.Fatalf("files=%v err=%v", files, err)
	}
}

func TestFingerprintFollowsDatabase(t *testing.T) {
	env := home(t, true)
	before := fingerprints(t, env)
	writeDB(t, dbPath(env), map[string]any{
		"composerData:chat-ide-0001": obj{"_v": 17, "name": "Fix login", "lastUpdatedAt": ms(at(4, 0, 0))},
	})
	after := fingerprints(t, env)
	if before["chat-ide-0001"] == after["chat-ide-0001"] {
		t.Errorf("fingerprint of chat-ide-0001 didn't change: %q", after["chat-ide-0001"])
	}
	if before["chat-cli-0002"] != after["chat-cli-0002"] {
		t.Errorf("fingerprint of chat-cli-0002 changed")
	}
}

func fingerprints(t *testing.T, env source.Env) map[string]string {
	files, err := Source{}.Discover(env)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, f := range files {
		m[strings.TrimSuffix(filepath.Base(f.Path), ".jsonl")] = f.Fingerprint
	}
	return m
}

func TestParseEditorChatMergesDatabase(t *testing.T) {
	env := home(t, true)
	s, entries := parse(t, env, "/chat-ide-0001.jsonl")
	if s.Agent != "cursor" || s.ID != "chat-ide-0001" || s.Cwd != "/Users/me/proj" || s.Title != "Fix login" {
		t.Errorf("session = %+v", s)
	}
	if s.Parent != "" || len(s.Lacks) != 0 || s.Hidden {
		t.Errorf("parent/lacks/hidden = %q %v %v", s.Parent, s.Lacks, s.Hidden)
	}
	if !s.Started.Equal(at(3, 0, 59)) || !s.Updated.Equal(at(3, 10, 0)) {
		t.Errorf("started/updated = %v %v", s.Started, s.Updated)
	}
	sourcetest.Equal(t, sourcetest.Lines(entries), []string{
		"0 injected <user_info>\nOS: darwin\n</user_info>",
		"1 user Fix the login bug",
		"2 assistant Let me look",
		`3 tool_call[Shell] {"command":"ls src"}`,
		"4 tool_result[Shell] login.ts",
		`5 tool_call[Read] {"path":"a.ts"}`,
		"6 tool_result[Read] export const a = 1",
		`7 tool_call[Grep] {"pattern":"login"}`,
		"8 tool_result[Grep] Timed out after 25s",
		"9 assistant Found it",
		"10 user now commit",
		"11 injected <manually_attached_skills>\nSkill body\n</manually_attached_skills>",
		`12 tool_call[StrReplace] {"path":"a.ts","old_string":"a","new_string":"b"}`,
		`13 tool_call[Shell] {"command":"git status"}`,
		`14 tool_call[Shell] {"command":"git commit"}`,
		"15 tool_result[Shell] [main abc] fix",
		"16 user Briefly inform the user about the task result",
		"17 assistant Done",
	})
	wantTimes := map[int]time.Time{
		0: at(3, 1, 2), 1: at(3, 1, 2), 3: at(3, 1, 5), 4: at(3, 1, 6), 8: at(3, 1, 9),
		10: at(3, 5, 10), 15: at(3, 5, 16), 16: at(3, 9, 30),
	}
	for i, want := range wantTimes {
		if !entries[i].Time.Equal(want) {
			t.Errorf("entry %d time = %v, want %v", i, entries[i].Time, want)
		}
	}
	for _, e := range entries {
		if e.Time.IsZero() {
			t.Errorf("entry %d has no time", e.Seq)
		}
	}
}

func TestParseSideChatDropsInheritedHistory(t *testing.T) {
	env := home(t, true)
	s, entries := parse(t, env, "/chat-side-0003.jsonl")
	if s.ID != "chat-side-0003" || s.Parent != "cursor:chat-ide-0001" || s.Title != "why that file?" {
		t.Errorf("session = %+v", s)
	}
	if s.Cwd != "Users-me-proj" {
		t.Errorf("cwd = %q, want the raw folder name", s.Cwd)
	}
	sourcetest.Equal(t, sourcetest.Lines(entries), []string{
		"0 injected <side_chat_boundary>\nSide chat boundary.\n</side_chat_boundary>\n</system_reminder>",
		"1 user why that file?",
		"2 assistant Because",
		`3 tool_call[Glob] {"glob_pattern":"*.ts"}`,
		`4 tool_result[Glob] {"directories":[{"absPath":"/x","files":[{"relPath":"a.ts"}]}]}`,
	})
	if !entries[1].Time.Equal(at(3, 20, 5)) {
		t.Errorf("user time = %v", entries[1].Time)
	}
}

func TestParseCommandLineChat(t *testing.T) {
	env := home(t, true)
	s, entries := parse(t, env, "/chat-cli-0002.jsonl")
	if s.Cwd != "/Users/me/old" || s.Title != "List the files" {
		t.Errorf("session = %+v", s)
	}
	if strings.Join(s.Lacks, ",") != model.LackToolResults {
		t.Errorf("lacks = %v", s.Lacks)
	}
	if !s.Started.Equal(time.UnixMilli(1792723260000).UTC()) || !s.Updated.Equal(time.UnixMilli(1792731300000).UTC()) {
		t.Errorf("started/updated = %v %v", s.Started, s.Updated)
	}
	sourcetest.Equal(t, sourcetest.Lines(entries), []string{
		"0 user List the files",
		"1 assistant plain string reply",
		`2 tool_call[Shell] {"command":"ls"}`,
		"3 user thanks",
	})
	// 11:01 at UTC+5:30 is 05:31 UTC; the time carries to the turn's replies.
	for i, want := range []time.Time{at(5, 31, 0), at(5, 31, 0), at(5, 31, 0), at(7, 45, 0)} {
		if !entries[i].Time.Equal(want) {
			t.Errorf("entry %d time = %v, want %v", i, entries[i].Time, want)
		}
	}
}

func TestParseWithoutDatabase(t *testing.T) {
	env := home(t, false)
	s, entries := parse(t, env, "/chat-ide-0001.jsonl")
	if strings.Join(s.Lacks, ",") != model.LackToolResults || s.Title != "Fix the login bug" {
		t.Errorf("session = %+v", s)
	}
	if s.Cwd != "Users-me-proj" {
		t.Errorf("cwd = %q", s.Cwd)
	}
	if len(entries) != 14 || !entries[1].Time.Equal(at(3, 1, 0)) {
		t.Errorf("got %d entries, entry 1 time %v", len(entries), entries[1].Time)
	}
	if !s.Started.Equal(at(3, 1, 0)) || !s.Updated.Equal(at(3, 9, 0)) {
		t.Errorf("started/updated = %v %v", s.Started, s.Updated)
	}
}

func TestParseFlatChatWithoutTimes(t *testing.T) {
	env := home(t, true)
	s, entries := parse(t, env, "/chat-flat-0004.jsonl")
	if s.ID != "chat-flat-0004" || strings.Join(s.Lacks, ",") != "tool_results,timestamps" {
		t.Errorf("session = %+v", s)
	}
	if s.Updated.IsZero() {
		t.Errorf("updated should fall back to the file's modified time")
	}
	sourcetest.Equal(t, sourcetest.Lines(entries), []string{"0 user hello there", "1 assistant hi"})
}

func TestDecodeFolderAgainstFileSystem(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"work/app-worktrees/fix-foo", "work/app/src", "work/.cfg", "work/My Notes~x"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Cursor turns each run of other characters into one dash:
	// /Users/me/.cursor becomes Users-me-cursor.
	enc := func(p string) string {
		var b strings.Builder
		for _, f := range strings.FieldsFunc(p, func(r rune) bool {
			return !(r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'))
		}) {
			if b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteString(f)
		}
		return b.String()
	}
	env := source.Env{Home: root}
	for _, want := range []string{
		root + "/work/app-worktrees/fix-foo",
		root + "/work/app/src",
		root + "/work/.cfg",
		root + "/work/My Notes~x",
	} {
		if got := decodeFolder(env, enc(want)); got != want {
			t.Errorf("decodeFolder(%q) = %q, want %q", enc(want), got, want)
		}
	}
	if got := decodeFolder(env, enc(root+"/work/gone")); got != "" {
		t.Errorf("missing folder decoded to %q", got)
	}
}
