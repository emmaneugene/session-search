# session-search design

`session-search` is a command-line tool, written in Go, that searches and reads past sessions from Pi, Cursor, Claude Code and Codex. Any agent in any of those tools can call it to find what an earlier session decided. It keeps a small SQLite full-text index and gives agents a fixed JSON output. This document records the design, the measurements behind its choices, and the format facts found while building the readers.

## Problem

Each coding agent keeps its sessions in its own folders and its own format, and none of them can search the others. For example, Pi has no reader for Cursor chats. An agent can grep the files, but only if it knows where they live and how each format nests its messages. The search tool that came with Pi ([pi-session-search](https://github.com/adobe/pi-session-search)) scans every file on every query, so a cold search over 2.5 GB of Pi sessions took about 80 seconds.

## Goals

- Search and read sessions from Pi, Cursor, Claude Code and Codex with one command, and make adding another agent a matter of adding one reader.
- Return a warm search in under a second on about 4 GB of transcripts.
- Give agents stable JSON for search and Markdown or JSON for reading, both with size limits.
- Never write to any agent's files, and run without a background process or network access.

Out of scope for the first version are a web or terminal interface, embeddings or semantic search, cost and token reports, and sync across machines. Semantic search gets added only if keyword search misses answers on the end-to-end questions.

## What the tool reads

The tool reads only each agent's default folders. It ignores settings that move them, e.g., Pi's `--session-dir` and `PI_CODING_AGENT_SESSION_DIR`, Codex's `CODEX_HOME`, and Claude Code's `CLAUDE_CONFIG_DIR`. Support for them can be added if someone needs it.

| Agent | Session files | Extra stores |
| --- | --- | --- |
| Pi | `~/.pi/agent/sessions/<cwd with dashes>/<timestamp>_<id>.jsonl`, plus `subagents/<parent id>/` | None |
| Cursor | `~/.cursor/projects/<cwd with dashes>/agent-transcripts/<id>/<id>.jsonl`, plus `<id>/subagents/`. Editor and command-line chats both write one. | `state.vscdb` holds titles, times and tool results for editor chats. `~/.config/cursor/chats/<hash>/<id>/meta.json` holds the working folder of command-line chats. |
| Claude Code | `~/.claude/projects/<cwd with dashes>/*.jsonl`, plus `<session id>/subagents/` | None |
| Codex | `~/.codex/sessions/YYYY/MM/DD/rollout-<time>-<id>.jsonl` and `~/.codex/archived_sessions/` | `~/.codex/session_index.jsonl` for titles |

The measurements in this document come from one Mac with 2,655 Pi sessions (2.5 GB), 421 Cursor transcripts (1.4 GB, 96 of them Cursor chats and the rest Cursor SDK runs), 35 Claude Code files and 93 Codex files.

Some places look like session stores but aren't. Cursor's `acp-sessions/` folder holds metadata only, and `workspaceStorage/*/state.vscdb` holds no chats. `~/.claude/history.jsonl` holds prompts only, and `~/.codex/thread_history_1.sqlite` is a copy of the Codex session files.

## References

The tool is new code. Other projects' code was read to learn each format, and no code was copied.

| Project | Licence | What it showed |
| --- | --- | --- |
| [agentsview](https://github.com/kenn-io/agentsview), `internal/parser/` | MIT | The main reference for all four formats, including Cursor subagent folders, the Cursor database, Codex archived sessions and titles, and Claude Code sidechains. Its test fixtures show many edge cases. |
| [SpecStory CLI](https://github.com/specstoryai/getspecstory), `specstory-cli/pkg/providers/` | Apache 2.0 | A second set of Go parsers, for cases where agentsview's code is unclear. |
| [vibe-replay notes on Cursor storage](https://vibe-replay.com/blog/cursor-local-storage/) | Not code | The clearest description of the Cursor stores. |
| [pi-session-search](https://github.com/adobe/pi-session-search) | | The search and read behaviour for Pi that this tool keeps. |

agentsview's parsers for these four agents come to about 15,000 lines, because they also cover usage, cost, cloud sources and file watching. session-search needs a much smaller part of that. agentsview also treats the Cursor database as a separate agent, so one editor chat shows up twice there. session-search merges the transcript and the database into one session that has the tool results.

The design keeps these lessons from the prior art:

- The transcript files stay the only source of truth, and the index is a cache that the tool can delete and rebuild.
- Output for agents needs a fixed shape, a size limit per call, paging, and a note on how fresh the index is.
- Each session says what its source lacks, e.g., "no tool results", instead of looking complete.
- Every agent injects `AGENTS.md` and skill text into user turns, so the index leaves that text out.

## Parts

1. Readers. Each agent has one reader in `internal/source/<agent>`. A reader lists its session files with a fingerprint (size and modified time), parses one file into a session and its entries, and returns one entry's text from its saved location in the file.
2. Index. A SQLite database at `~/.cache/session-search/index.db`, with file mode 0600. Before each command, the tool compares fingerprints with the index, reparses only the files that changed, and then runs the query. `index --rebuild` deletes the database and builds it again. A file lock makes sure only one process updates the index at a time.
3. Commands. `search` reads the index. `read` parses the transcript file itself, so it always shows the current file.

## SQLite driver

The driver choice mostly affects the first index build and hardly affects a warm search. The only public comparison found is [go-sqlite-bench](https://github.com/cvilsmeier/go-sqlite-bench) (March 2026, Linux on an Intel laptop, Go 1.26). It doesn't test FTS5, which is SQLite's full-text search, so its numbers are a rough guide.

| Test (milliseconds, lower is better) | `mattn/go-sqlite3` (cgo) | `modernc.org/sqlite` (pure Go) | `ncruces/go-sqlite3` (pure Go, WebAssembly) |
| --- | --- | --- | --- |
| Insert 1 million small rows | 1,480 | 2,419 | 2,719 |
| Insert 420,000 rows in 3 transactions | 812 | 1,554 | 1,749 |
| Query 1,000 small rows, 1,000 times | 819 | 692 | 837 |
| Read 10,000 rows of 200 KB each | 376 | 1,094 | 528 |

The pure Go drivers write about 1.6 to 1.9 times slower than the cgo driver, and they read small rows at about the same speed. A search reads a few dozen short rows, so it takes a few milliseconds with any of them.

The tool uses `modernc.org/sqlite`, because it needs no C compiler and gives one static binary. The first full build of all four agents takes 17 to 23 seconds, which is fast enough, so the tool doesn't need the cgo driver. All three drivers work through `database/sql`, so a switch would change one import and the build flags.

## Data model

A session has these fields:

- `agent` (pi, cursor, claude or codex) and `id`, which together form a reference such as `pi:01a16c01`
- `file`, `cwd`, `title`, `started` and `updated`
- `parent`, which is set for subagents and forks
- `lacks`, a list such as `["tool_results", "timestamps"]`
- `hidden`, which is set for sessions left out of results by default

An entry has `seq` (its position in the session), a time when the source has one, and a kind. The kind is one of `user`, `assistant`, `thinking`, `tool_call`, `tool_result`, `summary` or `injected` (system prompt, `AGENTS.md`, skill bodies). Entries also carry their text, the tool name for tool kinds, whether they sit on an abandoned branch, and their location in the file.

## Tool results in the index

Tool results are most of the text, so whether the index holds them sets its size and build time. A sample of 331 Pi sessions (one in eight, 215 MB of files) split like this:

| Content | Share of the text (images left out) | Scaled to all Pi sessions |
| --- | --- | --- |
| Tool results | 78% | about 945 MB |
| Tool call arguments | 11% | about 135 MB |
| Thinking | 6% | about 75 MB |
| Assistant and user text | 4% | about 50 MB |

The first 2 KB of every tool result add up to 27% of the tool result text, and the first 4 KB add up to 42%.

| Option | Text indexed for Pi | For | Against |
| --- | --- | --- | --- |
| A. No tool results | about 260 MB | Smallest index and fastest build. Searches match what people and agents wrote, so ranking stays clean. | Can't find an error message or command output by searching. |
| B. The first and last 1 KB of each result, in a separate table | about 520 MB | Catches most error lines and command summaries, which sit at the start or end of the output. A normal search ignores the separate table, so its speed and ranking don't change. | Double the index size and build time. Misses text in the middle of long outputs. |
| C. All tool results, in a separate table | about 1.2 GB | Finds anything any tool ever printed. | Biggest index and slowest build. Results fill up with file dumps and logs. |

The tool uses option B. A normal search stays as fast as option A, and `search --in tool-results` can still find most failed commands. The index uses FTS5 "contentless" tables, which keep only the word index and not a second copy of the text, so the tool builds result snippets by reading the matching lines back from the transcript files.

## Search ranking

Results come in three tiers. First come sessions where one message holds every query word, ranked by their best few messages. Second come sessions that hold every word spread over several messages. Third come sessions that hold only some of the words, ranked by how many. Without the middle tier, sessions that spread the query over several messages ranked below unrelated sessions.

Each session contributes at most a few entries to the candidate list, so one session with hundreds of matching messages can't push other matching sessions out of the results.

## Command line

```text
session-search search <query> [--agent pi,cursor,...] [--cwd TEXT] [--since DATE] [--until DATE]
                              [--include-tool-calls] [--include-thinking] [--in tool-results] [--limit N]
session-search read <agent:id | id | file | --current> [--at N | --around TIME | --offset N] [--context N]
                              [--max N] [--max-bytes N] [--include-tool-results] [--format md|json]
session-search list [--agent ...] [--cwd TEXT] [--since DATE] [--live]
session-search resolve <id or prefix>
session-search sources
session-search index [--rebuild]
```

`read --current` reads the session the caller runs in. It finds the session from `PI_SESSION_FILE` in Pi and from `CURSOR_CONVERSATION_ID` in Cursor. `search` leaves the current session out of its results.

JSON output is compact by default, and `--pretty` indents it. Search output has a `meta` block that says how fresh the index is and whether the output was cut short. The exit codes are 0 for success, 1 for no match, 2 for a usage error and 3 for any other error.

## Folder cache

Listing a folder takes about 0.5 ms on macOS, against about 2 µs to check one file's size and modified time. Walking every session folder before every command cost 330 ms for Pi alone. The index therefore saves each folder's listing together with the folder's modified time, and the tool lists a folder again only when that time changes. With the cache, the check before each command takes about 10 to 25 ms. Readers list folders only through `Env.ReadDir` and `Env.Walk`, so they all use the cache.

## Format facts per agent

### Pi

- Sessions are trees, because a user can branch from an earlier message. The reader follows the branch that ends at the last entry, and it marks entries on other branches as abandoned.
- The first line holds the working folder, and some folder names contain spaces.
- Subagent sessions sit in `<cwd folder>/subagents/<parent id>/`, so the reader walks the whole sessions folder.

### Cursor

- A transcript holds user text, assistant text and tool calls, but no tool results and no time per message. The folder name turns each run of characters other than letters and digits into one dash, so the working folder can't be decoded reliably from it.
- The editor database (`~/Library/Application Support/Cursor/User/globalStorage/state.vscdb`, table `cursorDiskKV`) uses the same chat id as the transcript. `composerData:<id>` holds the title and workspace, and `bubbleId:<id>:<bubble>` holds each message with its time and tool result. It covers editor chats only, and its layout can change with any Cursor update.
- The current layout keeps message order in `composerConvHeaders:<id>:<n>` rows, and older chats keep it in `fullConversationHeadersOnly`.
- Transcript tool calls have no ids, and the database uses different tool names and orders parallel calls differently. The reader matches calls within one user turn, and it prefers a record that shares an argument value such as a command or a path. On the test machine 99.5% of tool calls matched a record.
- The database keeps no result for most file reads and searches, so only about 1,600 of 4,300 matched tool calls get a result.
- Side chats repeat the parent's history before a `<side_chat_boundary>` block, and the reader drops that part. The same chat id can appear in two project folders, and the reader keeps the newest file.
- Transcripts named `agent-<uuid>` are Cursor SDK runs, which Pi's Cursor provider drives. The Pi session file holds the same conversation with tool results and times, so the reader skips these transcripts and never indexes them. On the test machine they were 325 of 421 transcripts. Every one of them began with Pi's Cursor provider prompt, and about 95% matched a Pi session by a link record or by the text of the first request.
- The reader opens the database read-only. SQLite's read-only mode can still create `-wal` and `-shm` files next to the database, so the reader opens it with `immutable=1` unless both files already exist.

### Claude Code

- Each content block of an assistant message is its own record. Parallel tool calls give each result a different parent, so results can sit off the main path without being abandoned.
- A compaction boundary has no `parentUuid` and links to its real parent through `logicalParentUuid`, so the branch walk follows both fields.
- Titles come from `custom-title`, then `ai-title`, then the first user text. Subagents use the `description` in their `.meta.json` file.
- A slash command with arguments counts as a user entry, because the arguments are the person's request.

### Codex

- The session id comes from `session_meta`, not the file name.
- `event_msg` repeats the `response_item` messages, so the reader uses `event_msg` only for files with no `response_item` messages.
- Review threads (`thread_source: guardian_review`) are hidden.
- Forks replay the parent's history at the start of the file with no second `session_meta`. The reader drops lines up to the first pause longer than 250 ms. A fork whose lines all share one millisecond keeps its replay, which is a known gap.
- Subagent tasks arrive encrypted, so subagent sessions are titled by nickname and path. `compacted.message` was empty in every file on the test machine.

## End-to-end test

The end-to-end test is a set of questions whose answers are known, spread over the four agents. Each question gives a keyword query and the sessions that answer it. The scorecard (`e2e/scorecard`) runs every question and records where the first expected session ranks, the search time and the output size. It can run agentsview on the same questions as a baseline.

The questions describe the author's own sessions, so they aren't in the repository. On the author's machine, with 17 questions, session-search ranked the right session first for 14 and in the top five for 16. agentsview ranked it first for 8 and in the top five for 14. The median search took 170 to 250 ms against about 320 ms for agentsview. The first full index build took 17 to 23 seconds and made a 190 MB index, against 160 seconds and 1.4 GB for agentsview. The one question that session-search missed has its answer in the middle of an 8 KB tool output, which option B doesn't index.
