# session-search

`session-search` searches and reads past AI coding agent sessions from Pi, Cursor, Claude Code and Codex. Any agent can call it to find what an earlier session decided, whichever tool that session ran in. It reads each agent's default folders, keeps a full-text index in `~/.cache/session-search`, and never writes to any agent's files.

```sh
session-search search sqlite busy timeout            # JSON, best sessions first
session-search read pi:0199aa12 --at 42              # Markdown around entry 42
session-search read --current                        # the session this command runs in
session-search list --agent cursor --since 2d        # recent sessions
session-search resolve 0199aa12                      # one session by id or id prefix
session-search sources                               # what each agent has, index state
```

Run `session-search --help` or `session-search <command> --help` for every flag.

## Install

```sh
make install                        # builds, then copies the binary to ~/.local/bin
make install BINDIR=/usr/local/bin  # or to any other folder
```

It's built and tested on macOS. It also builds for Linux, but it isn't tested there, and it looks for the Cursor editor database only at its macOS path. It doesn't build on Windows, because it locks the index with `flock`.

`make install` copies the binary, so the installed copy stays the same when you rebuild or clean the repo. It replaces any file or symlink already at `BINDIR/session-search` and creates `BINDIR` if it doesn't exist. The folder must be on your `PATH`. The binary is self-contained and doesn't need Go at run time. Run `make install` again after you pull or change the code.

## What it reads

| Agent | Session files | Extra stores |
| --- | --- | --- |
| Pi | `~/.pi/agent/sessions/**.jsonl`, including `subagents/<parent id>/` | None |
| Cursor | `~/.cursor/projects/*/agent-transcripts/<id>/<id>.jsonl`, plus `subagents/` | The editor database (`~/Library/Application Support/Cursor/User/globalStorage/state.vscdb`) adds tool results, times and titles. `~/.config/cursor/chats/*/<id>/meta.json` gives the working folder of command-line chats. |
| Claude Code | `~/.claude/projects/<slug>/*.jsonl`, plus `<session id>/subagents/` | None |
| Codex | `~/.codex/sessions/**` and `~/.codex/archived_sessions/` | `~/.codex/session_index.jsonl` for titles |

It reads only these default folders. Settings that move them (`PI_CODING_AGENT_SESSION_DIR`, `CODEX_HOME`, `CLAUDE_CONFIG_DIR`) are ignored.

Some sessions are hidden from results unless you pass `--include-hidden`: Codex automatic review threads, and sessions with no user message. Cursor `agent-<uuid>` transcripts aren't read at all. The Cursor SDK writes them when Pi runs through a Cursor provider, and the Pi session file holds the same conversation with tool results and times, so each conversation is counted once, as a Pi session.

## How search works

The index holds user and assistant messages, summaries, thinking, tool call arguments and the first and last kilobyte of each tool result. Injected text (system prompts, `AGENTS.md`, skill bodies, environment blocks) isn't indexed, because every session repeats it. The full-text tables keep only the word index, so snippets are read back from the transcript files.

Results come in three tiers, and each result says which one it's in:

1. `"matched": "message"`: one message holds every query word. These rank by their best few messages.
2. `"matched": "session"`: the session holds every word, spread over several messages.
3. `"matched": "some"`: the session holds some of the words, ranked by how many.

By default a search covers user and assistant messages and summaries. `--include-tool-calls` and `--include-thinking` add those kinds, and `--in tool-results` searches tool output instead. The current session (`$PI_SESSION_ID` or `$CURSOR_CONVERSATION_ID`) is left out.

Every command brings the index up to date first. It reparses only files whose size or modified time changed, and it reads a folder listing again only when the folder's modified time changed, because listing a folder on macOS takes about 0.5 ms and checking a file takes about 2 µs. A warm search takes 60 to 300 ms. The first build of about 3,200 sessions (4 GB) takes about 17 seconds and makes a 190 MB index.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Found something |
| 1 | No match, or a session reference matched nothing or several sessions |
| 2 | Usage error |
| 3 | Other error |

## Environment

| Variable | Effect |
| --- | --- |
| `SESSION_SEARCH_HOME` | Read agent folders under this folder instead of `$HOME` (tests use `testdata/home`) |
| `SESSION_SEARCH_INDEX` | Index file, default `~/.cache/session-search/index.db` |
| `SESSION_SEARCH_DEBUG` | Print open and sync times to stderr |

## Development

Go 1.27.3, pinned in `mise.toml`.

```sh
make build                                              # writes bin/session-search
make test                                               # reader fixtures and CLI end-to-end tests
SESSION_SEARCH_REAL=1 mise exec -- go test ./internal/source/...   # parse every real session
mise exec -- go run ./e2e/scorecard                     # your own questions, see below
```

The scorecard checks search quality on your real sessions. Write questions whose answers you know in `eval/questions.json`, which git ignores because the questions describe your own work. The format is in the comment at the top of `e2e/scorecard/main.go`. For each question, the scorecard records where the expected session ranks, the search time and the output size, and it writes a report to `eval/out/`. `--agentsview PATH` runs [agentsview](https://github.com/kenn-io/agentsview) on the same questions as a baseline (its daemon must be running).

Each reader lives in `internal/source/<agent>` and implements `source.Source`: `Discover` lists session files with a fingerprint, `Parse` turns one file into a session and its entries, and `Text` returns one entry's text from its saved location without parsing the whole file. Readers list folders only through `Env.ReadDir` and `Env.Walk`, so they use the folder cache.

The session formats were worked out from the real files, with [agentsview's parsers](https://github.com/kenn-io/agentsview/tree/main/internal/parser) (MIT), [SpecStory CLI](https://github.com/specstoryai/getspecstory) and the [vibe-replay notes on Cursor storage](https://vibe-replay.com/blog/cursor-local-storage/) as references. `docs/design.md` has the design, the measurements behind it and the format facts for each agent.

## Licence

MIT. See `LICENSE`.
