// Command scorecard runs end-to-end questions against the real session
// folders and writes a scorecard. The questions describe one person's own
// sessions, so they live in eval/, which git ignores.
//
// Each question has a keyword query and the session(s) that answer it. The
// scorecard records where the first expected session ranks, the search time
// and the output size. It can run agentsview on the same questions as a
// baseline.
//
//	go run ./e2e/scorecard                      # session-search only
//	go run ./e2e/scorecard --agentsview PATH    # also agentsview
//
// The questions file looks like this. "until" freezes the corpus: sessions
// started after it, such as the ones that wrote the questions, are left out.
//
//	{"until": "2026-10-24T03:30:00Z", "questions": [
//	  {"id": "babysit-skill", "agent": "codex", "query": "babysit pr skill",
//	   "in": "text", "expect": ["codex:019e..."], "note": "why this answers it"}
//	]}
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type question struct {
	ID     string   `json:"id"`
	Agent  string   `json:"agent"`
	Query  string   `json:"query"`
	In     string   `json:"in"`
	Expect []string `json:"expect"`
	Note   string   `json:"note"`
}

type outcome struct {
	Tool    string   `json:"tool"`
	ID      string   `json:"id"`
	Agent   string   `json:"agent"`
	Query   string   `json:"query"`
	In      string   `json:"in"`
	Rank    int      `json:"rank"` // 1-based; 0 means not in the results
	Top     []string `json:"top"`
	Millis  int64    `json:"ms"`
	Bytes   int      `json:"bytes"`
	Error   string   `json:"error,omitempty"`
	Expects []string `json:"expect"`
}

type questionFile struct {
	Until     string     `json:"until"`
	Questions []question `json:"questions"`
}

// cutoff is the questions file's "until" time.
var cutoff string

func main() {
	bin := flag.String("bin", "bin/session-search", "session-search binary")
	qfile := flag.String("questions", "eval/questions.json", "questions file")
	outDir := flag.String("out", "eval/out", "output folder")
	av := flag.String("agentsview", "", "agentsview binary for a baseline run (optional)")
	runs := flag.Int("runs", 3, "timed runs per question; the median is reported")
	flag.Parse()

	var qf questionFile
	data, err := os.ReadFile(*qfile)
	must(err)
	must(json.Unmarshal(data, &qf))
	if _, err := time.Parse(time.RFC3339, qf.Until); err != nil {
		must(fmt.Errorf("%s: \"until\" must be an RFC 3339 time: %v", *qfile, err))
	}
	cutoff = qf.Until
	qs := qf.Questions

	var all []outcome
	for _, q := range qs {
		all = append(all, run(q, *runs, func() ([]byte, error) { return ours(*bin, q) }, parseOurs, "session-search"))
		if *av != "" {
			all = append(all, run(q, *runs, func() ([]byte, error) { return agentsview(*av, q) }, parseAgentsview, "agentsview"))
		}
	}

	must(os.MkdirAll(*outDir, 0o755))
	stamp := time.Now().Format("2006-01-02T15-04")
	js, _ := json.MarshalIndent(all, "", "  ")
	jsonPath := filepath.Join(*outDir, "scorecard-"+stamp+".json")
	must(os.WriteFile(jsonPath, js, 0o644))
	md := report(all, qs)
	mdPath := filepath.Join(*outDir, "scorecard-"+stamp+".md")
	must(os.WriteFile(mdPath, []byte(md), 0o644))
	must(os.WriteFile(filepath.Join(*outDir, "latest.md"), []byte(md), 0o644))
	fmt.Print(md)
	fmt.Printf("\nwrote %s and %s\n", jsonPath, mdPath)
}

func run(q question, runs int, exec func() ([]byte, error), parse func([]byte) ([]string, error), tool string) outcome {
	o := outcome{Tool: tool, ID: q.ID, Agent: q.Agent, Query: q.Query, In: q.In, Expects: q.Expect}
	var times []int64
	var out []byte
	var err error
	for range max(runs, 1) {
		start := time.Now()
		out, err = exec()
		times = append(times, time.Since(start).Milliseconds())
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	o.Millis = times[len(times)/2]
	o.Bytes = len(out)
	if err != nil && len(out) == 0 {
		o.Error = err.Error()
		return o
	}
	refs, perr := parse(out)
	if perr != nil {
		o.Error = perr.Error()
		return o
	}
	for i, r := range refs {
		if i < 5 {
			o.Top = append(o.Top, r)
		}
		for _, e := range q.Expect {
			if o.Rank == 0 && (r == e || strings.HasSuffix(e, ":"+r)) {
				o.Rank = i + 1
			}
		}
	}
	return o
}

func ours(bin string, q question) ([]byte, error) {
	args := []string{"search", q.Query, "--limit", "10", "--until", cutoff}
	if q.In == "tool-results" {
		args = append(args, "--in", "tool-results")
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "PI_SESSION_ID=", "CURSOR_CONVERSATION_ID=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && stderr.Len() > 0 {
		err = fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, err
}

func parseOurs(out []byte) ([]string, error) {
	var r struct {
		Results []struct {
			Ref string `json:"ref"`
		} `json:"results"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, err
	}
	var refs []string
	for _, x := range r.Results {
		refs = append(refs, x.Ref)
	}
	return refs, nil
}

func agentsview(bin string, q question) ([]byte, error) {
	// agentsview's --date-to takes a day, so the day before the cutoff
	// keeps out the sessions that built this tool. --fts is its keyword
	// search over messages; without it a query is one literal pattern,
	// which is also the only way it searches tool output.
	args := []string{"session", "search", q.Query, "--json", "--limit", "50", "--date-to", "2026-10-23", "--include-children", "--include-automated", "--include-one-shot"}
	if q.In == "tool-results" {
		args = append(args, "--in", "tool_result")
	} else {
		args = append(args, "--fts")
	}
	cmd := exec.Command(bin, args...)
	// agentsview's search needs its daemon running (agentsview daemon start).
	cmd.Env = append(os.Environ(), "DO_NOT_TRACK=1", "AGENTSVIEW_TELEMETRY_ENABLED=0")
	return cmd.Output()
}

// parseAgentsview reads session ids in result order, keeping the first
// occurrence of each session. agentsview's JSON shape is read loosely: any
// object with a session id field counts.
func parseAgentsview(out []byte) ([]string, error) {
	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, err
	}
	var refs []string
	seen := map[string]bool{}
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			for _, k := range []string{"session_id", "sessionId", "session"} {
				if s, ok := t[k].(string); ok && s != "" {
					s = avID(s)
					if !seen[s] {
						seen[s] = true
						refs = append(refs, s)
					}
					break
				}
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(t[k])
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return refs, nil
}

// avID strips agentsview's agent prefixes ("cursor-ide:", "codex:") so the
// bare id can match an expected ref's id.
func avID(s string) string {
	if i := strings.LastIndex(s, ":"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func report(all []outcome, qs []question) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# session-search scorecard\n\nRun %s. Corpus: sessions started before %s. Rank is the position of the first expected session (0 means not in the top 10 for session-search, or the first 50 for agentsview).\n\n", time.Now().Format("2006-01-02 15:04"), cutoff)
	tools := map[string][]outcome{}
	var names []string
	for _, o := range all {
		if _, ok := tools[o.Tool]; !ok {
			names = append(names, o.Tool)
		}
		tools[o.Tool] = append(tools[o.Tool], o)
	}
	b.WriteString("| Tool | Questions | Top 1 | Top 5 | Found | Mean reciprocal rank | Median ms | Median KB |\n| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, n := range names {
		outs := tools[n]
		top1, top5, found := 0, 0, 0
		mrr := 0.0
		var ms, kb []float64
		for _, o := range outs {
			if o.Rank == 1 {
				top1++
			}
			if o.Rank >= 1 && o.Rank <= 5 {
				top5++
			}
			if o.Rank > 0 {
				found++
				mrr += 1 / float64(o.Rank)
			}
			ms = append(ms, float64(o.Millis))
			kb = append(kb, float64(o.Bytes)/1024)
		}
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %.2f | %.0f | %.1f |\n", n, len(outs), top1, top5, found, mrr/float64(len(outs)), median(ms), median(kb))
	}
	b.WriteString("\n| Question | Agent | In | Query |")
	for _, n := range names {
		fmt.Fprintf(&b, " %s rank | %s ms |", n, n)
	}
	b.WriteString("\n| --- | --- | --- | --- |")
	for range names {
		b.WriteString(" --- | --- |")
	}
	b.WriteString("\n")
	for _, q := range qs {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |", q.ID, q.Agent, q.In, q.Query)
		for _, n := range names {
			for _, o := range tools[n] {
				if o.ID == q.ID {
					rank := fmt.Sprint(o.Rank)
					if o.Error != "" {
						rank = "error"
					}
					fmt.Fprintf(&b, " %s | %d |", rank, o.Millis)
				}
			}
		}
		b.WriteString("\n")
	}
	var misses []string
	for _, o := range all {
		if o.Rank == 0 || o.Rank > 5 {
			misses = append(misses, fmt.Sprintf("- %s / %s: rank %d, expected %s, top %v %s", o.Tool, o.ID, o.Rank, strings.Join(o.Expects, " or "), o.Top, o.Error))
		}
	}
	if len(misses) > 0 {
		b.WriteString("\nMisses (not in the top 5):\n\n" + strings.Join(misses, "\n") + "\n")
	}
	return b.String()
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	return xs[len(xs)/2]
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "scorecard:", err)
		os.Exit(1)
	}
}
