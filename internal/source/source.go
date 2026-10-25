// Package source defines the interface each agent reader implements, and
// helpers shared by readers of JSONL transcripts.
package source

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/emmaneugene/session-search/internal/model"
)

// Env is what a reader needs to find files. Home replaces the user's home
// folder, so tests can point every reader at a fixture tree. Dirs, when
// set, caches folder listings between runs; readers list folders through
// Env.ReadDir and Env.Walk so they benefit from it.
type Env struct {
	Home string
	Dirs *DirCache
}

// Path joins parts under Home.
func (e Env) Path(parts ...string) string {
	return filepath.Join(append([]string{e.Home}, parts...)...)
}

// File is one session file found by Discover. Fingerprint changes whenever
// the session's content may have changed. For a plain file it's the size
// and modified time. A reader that merges another store into the session
// adds that store's change marker.
type File struct {
	Path        string
	Fingerprint string
}

// Source reads one agent's sessions.
type Source interface {
	// Agent is the short name used in references, such as "pi".
	Agent() string
	// Discover lists every session file under the agent's default folders.
	// A missing folder is not an error.
	Discover(env Env) ([]File, error)
	// Parse reads one session file into a session and its entries, in order.
	// Entries carry their full text and a Loc for Text.
	Parse(env Env, path string) (model.Session, []model.Entry, error)
	// Text returns the full text of one entry from its Loc, without parsing
	// the whole file.
	Text(env Env, path string, loc model.Loc) (string, error)
}

// StatFingerprint is the fingerprint of a plain file: its size and modified
// time.
func StatFingerprint(info os.FileInfo) string {
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

// Line is one line of a JSONL file and where it starts.
type Line struct {
	Offset int64
	Data   []byte // without the trailing newline; valid only during the callback
}

// EachLine calls fn for each non-empty line of a JSONL file. Lines of any
// length are supported, because transcripts embed images and large tool
// outputs on one line.
func EachLine(path string, fn func(Line) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var offset int64
	for {
		data, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// Long line: collect the rest of it.
			buf := append([]byte(nil), data...)
			for errors.Is(err, bufio.ErrBufferFull) {
				data, err = r.ReadSlice('\n')
				buf = append(buf, data...)
			}
			data = buf
		}
		n := int64(len(data))
		line := trimNewline(data)
		if len(line) > 0 {
			if ferr := fn(Line{Offset: offset, Data: line}); ferr != nil {
				return ferr
			}
		}
		offset += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// ReadLine reads the line at offset with the given length.
func ReadLine(path string, offset int64, length int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, length)
	if _, err := f.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf, nil
}

// LineLoc is the Loc for part n of a line.
func LineLoc(l Line, part int) model.Loc {
	return model.Loc{Offset: l.Offset, Length: len(l.Data), Part: part}
}
