package cursor

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/emmaneugene/session-search/internal/source"
)

// Cursor names a project folder after its path with each run of characters
// other than ASCII letters and digits turned into one dash, so
// /Users/me/.cursor becomes Users-me-cursor and app-worktrees/fix-foo reads
// the same as app/worktrees-fix/foo. decodeFolder finds the existing folder
// whose name encodes to the given one, trying each split against the real
// file system. It returns "" when none exists.
func decodeFolder(env source.Env, name string) string {
	key := env.Home + "\x00" + name
	if v, ok := decoded.Load(key); ok {
		return v.(string)
	}
	p := decodeUnder(env, "/", name)
	decoded.Store(key, p)
	return p
}

// decoded caches decodeFolder per process: many chats share a folder, and
// each step lists a directory.
var decoded sync.Map

func decodeUnder(env source.Env, dir, rest string) string {
	items, err := env.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, it := range items {
		e := encodeName(it.Name)
		if e == "" || !strings.HasPrefix(rest, e) {
			continue
		}
		tail := rest[len(e):]
		if tail != "" && tail[0] != '-' {
			continue
		}
		p := filepath.Join(dir, it.Name)
		if info, err := os.Stat(p); err != nil || !info.IsDir() {
			continue
		}
		if tail == "" {
			return p
		}
		if found := decodeUnder(env, p, tail[1:]); found != "" {
			return found
		}
	}
	return ""
}

func encodeName(name string) string {
	return strings.Join(strings.FieldsFunc(name, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	}), "-")
}
