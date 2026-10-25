// Regression tests for the findings of the 2026-10-24 code review.
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writePi writes a Pi session with the given user messages.
func writePi(t *testing.T, home, id string, msgs []string) string {
	t.Helper()
	dir := filepath.Join(home, ".pi/agent/sessions/--Users-me-review--")
	os.MkdirAll(dir, 0o755)
	var b strings.Builder
	fmt.Fprintf(&b, `{"type":"session","version":3,"id":%q,"timestamp":"2026-10-05T00:00:00.000Z","cwd":"/Users/me/review"}`+"\n", id)
	parent := "null"
	for i, m := range msgs {
		eid := fmt.Sprintf("e%d", i)
		fmt.Fprintf(&b, `{"type":"message","id":%q,"parentId":%s,"timestamp":"2026-10-05T00:%02d:%02d.000Z","message":{"role":"user","content":%q}}`+"\n",
			eid, parent, i/60%60, i%60, m)
		parent = fmt.Sprintf("%q", eid)
	}
	path := filepath.Join(dir, "2026-10-05T00-00-00-000Z_"+id+".jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

type window struct {
	First   int `json:"first"`
	Total   int `json:"total"`
	Entries []struct {
		Seq  int    `json:"seq"`
		Text string `json:"text"`
	} `json:"entries"`
	Truncated bool `json:"truncated"`
}

func TestReadWindowKeepsTheRequestedEntry(t *testing.T) {
	e := newEnv(t)
	var msgs []string
	for i := range 30 {
		msgs = append(msgs, fmt.Sprintf("message %d", i))
	}
	writePi(t, e.home, "sess-window-0005", msgs)
	var w window
	e.json(&w, "read", "pi:sess-window-0005", "--at", "20", "--max", "1", "--format", "json")
	if len(w.Entries) != 1 || w.Entries[0].Seq != 20 {
		t.Errorf("--at 20 --max 1 gave %+v", w.Entries)
	}
	e.json(&w, "read", "pi:sess-window-0005", "--at", "29", "--context", "2", "--format", "json")
	// Near the end the window keeps its size and still includes entry 29.
	if len(w.Entries) != 5 || w.Entries[4].Seq != 29 {
		t.Errorf("--at 29 near the end gave %+v", w.Entries)
	}
	e.json(&w, "read", "pi:sess-window-0005", "--offset", "999", "--format", "json")
	if len(w.Entries) != 0 {
		t.Errorf("--offset past the end gave %+v", w.Entries)
	}
}

func TestReadMaxBytesBoundsOutput(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("word ", 2000)
	writePi(t, e.home, "sess-bytes-0006", []string{long, long, long})
	for _, format := range []string{"json", "md"} {
		out, _, code := e.run("read", "pi:sess-bytes-0006", "--max-bytes", "3000", "--format", format)
		if code != 0 || len(out) > 3000 {
			t.Errorf("%s: exit %d, %d bytes with --max-bytes 3000", format, code, len(out))
		}
	}
	if _, _, code := e.run("read", "pi:sess-bytes-0006", "--max-bytes", "1"); code != 2 {
		t.Errorf("--max-bytes 1: exit %d, want usage error", code)
	}
}

func TestBusySessionDoesNotHideOthers(t *testing.T) {
	e := newEnv(t)
	var many []string
	for range 500 {
		many = append(many, "rareword")
	}
	writePi(t, e.home, "sess-busy-0007", many)
	writePi(t, e.home, "sess-quiet-0008", []string{"a longer message that also mentions the rareword once among many other words"})
	var r searchOut
	e.json(&r, "search", "rareword")
	refs := map[string]bool{}
	for _, x := range r.Results {
		refs[x.Ref] = true
	}
	if !refs["pi:sess-busy-0007"] || !refs["pi:sess-quiet-0008"] {
		t.Errorf("results = %v", refs)
	}
}

func TestParseFailureKeepsLastGoodEntries(t *testing.T) {
	e := newEnv(t)
	path := writePi(t, e.home, "sess-fail-0009", []string{"pelican"})
	var r searchOut
	e.json(&r, "search", "pelican")
	if len(r.Results) != 1 {
		t.Fatalf("before: %+v", r.Results)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"message","id":"x","parentId":"e0","timestamp":"2026-10-05T01:00:00.000Z","message":{"role":"user","content":"flamingo"}}` + "\n")
	f.Close()
	os.Chmod(path, 0)
	defer os.Chmod(path, 0o644)
	e.json(&r, "search", "pelican")
	if len(r.Results) != 1 {
		t.Errorf("after a failed parse the old entries are gone: %+v", r.Results)
	}
	// A change retries at once.
	os.Chmod(path, 0o644)
	future := time.Now().Add(time.Minute)
	os.Chtimes(path, future, future)
	e.json(&r, "search", "flamingo")
	if len(r.Results) != 1 {
		t.Errorf("not retried after the file changed: %+v", r.Results)
	}
}

func TestRebuildWaitsForTheLock(t *testing.T) {
	e := newEnv(t)
	e.run("index")
	lock, err := os.OpenFile(e.index+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "index", "--rebuild")
	cmd.Env = append(os.Environ(), "SESSION_SEARCH_HOME="+e.home, "SESSION_SEARCH_INDEX="+e.index)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		t.Fatal("rebuild ran while another process held the lock")
	case <-time.After(700 * time.Millisecond):
	}
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	lock.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("rebuild failed after the lock was released: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("rebuild never finished")
	}
	var s struct {
		Agents []struct {
			Agent   string `json:"agent"`
			Indexed int    `json:"indexed"`
		} `json:"agents"`
	}
	out, _, _ := e.run("sources")
	json.Unmarshal([]byte(out), &s)
	if len(s.Agents) == 0 || s.Agents[0].Indexed == 0 {
		t.Errorf("index empty after rebuild: %s", out)
	}
}
