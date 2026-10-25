// Package sourcetest holds checks that every source reader's tests share.
package sourcetest

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/emmaneugene/session-search/internal/model"
	"github.com/emmaneugene/session-search/internal/source"
)

// Env points at the shared fixture home, testdata/home.
func Env(t *testing.T) source.Env {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return source.Env{Home: filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "home")}
}

// Lines renders entries one per line, as "seq kind[tool][!abandoned] text",
// so a test can compare a whole session against a readable expectation.
func Lines(entries []model.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		kind := string(e.Kind)
		if e.Tool != "" {
			kind += "[" + e.Tool + "]"
		}
		if e.Abandoned {
			kind += "!abandoned"
		}
		out = append(out, fmt.Sprintf("%d %s %s", e.Seq, kind, e.Text))
	}
	return out
}

// Equal fails the test with a readable diff when the lines differ.
func Equal(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") == strings.Join(want, "\n") {
		return
	}
	t.Errorf("entries differ\n--- got\n%s\n--- want\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
}

// CheckText checks that Text returns each entry's full text from its Loc,
// and that Seq numbers run 0, 1, 2 and so on.
func CheckText(t *testing.T, src source.Source, env source.Env, path string, entries []model.Entry) {
	t.Helper()
	for i, e := range entries {
		if e.Seq != i {
			t.Errorf("entry %d has seq %d", i, e.Seq)
		}
		got, err := src.Text(env, path, e.Loc)
		if err != nil {
			t.Errorf("Text(seq %d): %v", e.Seq, err)
			continue
		}
		if got != e.Text {
			t.Errorf("Text(seq %d) = %q, want %q", e.Seq, got, e.Text)
		}
	}
}

// Find returns the discovered file whose path ends with suffix.
func Find(t *testing.T, files []source.File, suffix string) source.File {
	t.Helper()
	for _, f := range files {
		if strings.HasSuffix(f.Path, suffix) {
			return f
		}
	}
	t.Fatalf("no discovered file ends with %s", suffix)
	return source.File{}
}
