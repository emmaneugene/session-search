// End-to-end tests of the built binary against the fixture home in
// testdata/home. Each test copies the fixtures to a temporary home so it can
// change files and watch the index follow.
package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "session-search-e2e")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "session-search")
	build := exec.Command("go", "build", "-o", bin, "./cmd/session-search")
	build.Dir = repoRoot()
	if out, err := build.CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(file))
}

type env struct {
	t     *testing.T
	home  string
	index string
}

// newEnv copies testdata/home into a temporary home.
func newEnv(t *testing.T) *env {
	t.Helper()
	home := t.TempDir()
	src := filepath.Join(repoRoot(), "testdata", "home")
	cp := exec.Command("cp", "-R", src+"/.", home)
	if out, err := cp.CombinedOutput(); err != nil {
		t.Fatalf("copy fixtures: %v %s", err, out)
	}
	return &env{t: t, home: home, index: filepath.Join(t.TempDir(), "index.db")}
}

func (e *env) run(args ...string) (string, string, int) {
	e.t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "SESSION_SEARCH_HOME="+e.home, "SESSION_SEARCH_INDEX="+e.index,
		"PI_SESSION_ID=", "PI_SESSION_FILE=", "CURSOR_CONVERSATION_ID=")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return out.String(), errOut.String(), code
}

func (e *env) json(v any, args ...string) int {
	e.t.Helper()
	out, errOut, code := e.run(args...)
	if err := json.Unmarshal([]byte(out), v); err != nil {
		e.t.Fatalf("%v: output is not JSON: %v\nstdout: %s\nstderr: %s", args, err, out, errOut)
	}
	return code
}

type searchOut struct {
	Meta struct {
		Index struct {
			Reindexed int `json:"reindexed"`
		} `json:"index"`
	} `json:"meta"`
	Results []struct {
		Ref     string `json:"ref"`
		Matched string `json:"matched"`
		Matches []struct {
			Seq     int    `json:"seq"`
			Kind    string `json:"kind"`
			Snippet string `json:"snippet"`
		} `json:"matches"`
	} `json:"results"`
}

func TestSearchFindsAndRanks(t *testing.T) {
	e := newEnv(t)
	var r searchOut
	if code := e.json(&r, "search", "approach", "B", "--agent", "pi"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(r.Results) == 0 || r.Results[0].Ref != "pi:sess-main-0001" || r.Results[0].Matched != "message" {
		t.Fatalf("results = %+v", r.Results)
	}
	if got := r.Results[0].Matches[0]; got.Snippet != "Try approach B" && got.Snippet != "Approach B worked" {
		t.Errorf("first match = %+v", got)
	}
}

func TestSearchFallsBackToSomeWords(t *testing.T) {
	e := newEnv(t)
	var r searchOut
	e.json(&r, "search", "login", "cookies", "--agent", "pi")
	refs := map[string]string{}
	for _, x := range r.Results {
		refs[x.Ref] = x.Matched
	}
	// The subagent holds both words in one message; the main session
	// holds only "login".
	if refs["pi:sess-sub-0002"] != "message" || refs["pi:sess-main-0001"] != "some" {
		t.Errorf("matched = %v", refs)
	}
}

func TestToolResultsAreSeparate(t *testing.T) {
	e := newEnv(t)
	var r searchOut
	if code := e.json(&r, "search", "login.ts", "--agent", "pi"); code != 1 || len(r.Results) != 0 {
		t.Errorf("text search found tool output: exit %d, %+v", code, r.Results)
	}
	e.json(&r, "search", "login.ts", "--in", "tool-results", "--agent", "pi")
	if len(r.Results) != 1 || r.Results[0].Matches[0].Kind != "tool_result" {
		t.Errorf("tool-result search = %+v", r.Results)
	}
}

func TestIndexFollowsFileChanges(t *testing.T) {
	e := newEnv(t)
	var r searchOut
	e.json(&r, "search", "approach")
	file := filepath.Join(e.home, ".pi/agent/sessions/--Users-me-proj--/2026-10-02T00-00-00-000Z_sess-fork-0003.jsonl")

	// Appending to a session reindexes just that file.
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"message","id":"u2","parentId":"u1","timestamp":"2026-10-02T00:00:02.000Z","message":{"role":"user","content":"zebra crossing","timestamp":1790899202000}}` + "\n")
	f.Close()
	future := time.Now().Add(time.Minute)
	os.Chtimes(file, future, future)
	e.json(&r, "search", "zebra")
	if r.Meta.Index.Reindexed != 1 || len(r.Results) != 1 || r.Results[0].Ref != "pi:sess-fork-0003" {
		t.Fatalf("after append: reindexed %d, results %+v", r.Meta.Index.Reindexed, r.Results)
	}

	// A new file in a new folder is found.
	dir := filepath.Join(e.home, ".pi/agent/sessions/--Users-me-other--")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "2026-10-03T00-00-00-000Z_sess-new-0004.jsonl"), []byte(
		`{"type":"session","version":3,"id":"sess-new-0004","timestamp":"2026-10-03T00:00:00.000Z","cwd":"/Users/me/other"}`+"\n"+
			`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-10-03T00:00:01.000Z","message":{"role":"user","content":"giraffe","timestamp":1790985601000}}`+"\n"), 0o644)
	e.json(&r, "search", "giraffe")
	if len(r.Results) != 1 || r.Results[0].Ref != "pi:sess-new-0004" {
		t.Fatalf("new file: %+v", r.Results)
	}

	// A deleted file drops out.
	os.Remove(file)
	if code := e.json(&r, "search", "zebra"); code != 1 || len(r.Results) != 0 {
		t.Fatalf("after delete: exit %d, %+v", code, r.Results)
	}
}

func TestReadWindow(t *testing.T) {
	e := newEnv(t)
	out, _, code := e.run("read", "pi:sess-main", "--at", "8", "--context", "1")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{"# pi:sess-main-0001 Login fix", "## seq 7 summary", "## seq 8 user", "Try approach B", "## seq 10 tool_call bash"} {
		if !strings.Contains(out, want) {
			t.Errorf("read output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Try approach A") || strings.Contains(out, "login.ts") {
		t.Errorf("read shows abandoned entries or tool output by default:\n%s", out)
	}
	out, _, _ = e.run("read", "pi:sess-main", "--include-tool-results", "--include-abandoned", "--format", "json")
	var w struct {
		Total   int `json:"total"`
		Entries []struct {
			Kind string `json:"kind"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &w); err != nil || w.Total != 12 {
		t.Errorf("json read: total %d, err %v", w.Total, err)
	}
}

func TestResolveAndExitCodes(t *testing.T) {
	e := newEnv(t)
	var s struct {
		Ref    string `json:"ref"`
		Parent string `json:"parent"`
	}
	if code := e.json(&s, "resolve", "sess-sub"); code != 0 || s.Ref != "pi:sess-sub-0002" || s.Parent != "pi:sess-main-0001" {
		t.Errorf("resolve sess-sub: exit %d, %+v", code, s)
	}
	if _, errOut, code := e.run("resolve", "pi:sess-"); code != 1 || !strings.Contains(errOut, "several sessions") {
		t.Errorf("ambiguous prefix: exit %d, %s", code, errOut)
	}
	if _, _, code := e.run("resolve", "nope-nope-nope"); code != 1 {
		t.Errorf("unknown id: exit %d", code)
	}
	if _, _, code := e.run("search"); code != 2 {
		t.Errorf("search without query: exit %d", code)
	}
	if _, _, code := e.run("search", "x", "--bogus"); code != 2 {
		t.Errorf("unknown flag: exit %d", code)
	}
	if _, _, code := e.run("search", "x", "--agent", "vim"); code != 2 {
		t.Errorf("unknown agent: exit %d", code)
	}
	if out, _, code := e.run("search", "--help"); code != 0 || !strings.Contains(out, "Usage: session-search search") {
		t.Errorf("search --help: exit %d", code)
	}
}

func TestListAndCurrentSessionExcluded(t *testing.T) {
	e := newEnv(t)
	var l struct {
		Sessions []struct {
			Ref string `json:"ref"`
		} `json:"sessions"`
	}
	e.json(&l, "list", "--agent", "pi")
	if len(l.Sessions) != 3 || l.Sessions[0].Ref != "pi:sess-fork-0003" {
		t.Errorf("list = %+v", l.Sessions)
	}
	cmd := exec.Command(bin, "search", "approach")
	cmd.Env = append(os.Environ(), "SESSION_SEARCH_HOME="+e.home, "SESSION_SEARCH_INDEX="+e.index, "PI_SESSION_ID=sess-main-0001")
	out, _ := cmd.Output()
	if strings.Contains(string(out), "sess-main-0001") {
		t.Errorf("current session not excluded: %s", out)
	}
}
