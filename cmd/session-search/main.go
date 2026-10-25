// Command session-search searches and reads past AI coding agent sessions.
package main

import (
	"os"

	"github.com/emmaneugene/session-search/internal/cli"
	"github.com/emmaneugene/session-search/internal/source"
	"github.com/emmaneugene/session-search/internal/source/claude"
	"github.com/emmaneugene/session-search/internal/source/codex"
	"github.com/emmaneugene/session-search/internal/source/cursor"
	"github.com/emmaneugene/session-search/internal/source/pi"
)

func main() {
	srcs := []source.Source{pi.Source{}, cursor.Source{}, claude.Source{}, codex.Source{}}
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr, srcs))
}
