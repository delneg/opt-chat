# Working on opt-chat

Read [README.md](README.md) first, then the [design notes](docs/2026-10-12-design.md)
for why the code does what it does. Dated docs in `docs/` record decisions
as of their date; add a new dated file rather than rewriting history.

## This repo is public

Everything committed here, history included, is on public GitHub and stays
there: a commit can't be taken back without rewriting history. Never commit:

- anything from a real chat: messages, summary lines, imported sessions,
  eval questions, gold answers or model answers, screenshots of the page,
  project or people's names taken from it;
- personal data: emails, phone numbers, home paths with a user name,
  account ids;
- secrets: API keys, tokens, cookies, the page's `?t=` key.

Eval data and anything else drawn from a chat go in `private/`, which git
ignores. Test fixtures are made up (`/Users/me`, `me@example.com`). Before
a push, `gitleaks git .` must find nothing.

## Build and test

```sh
go build -o opt-chat .
go test ./...          # a few minutes
go test -short ./...   # skips the fsync-heavy tests
gofmt -l .             # must print nothing
```

The tests never call the real `claude`: the test binary plays it, speaking
stream-json (see `fakeChat`, `fakeAgent` and `TestMain` in the tests).
`TestMain` points `CLAUDE_CONFIG_DIR` at a temp dir, so subagent sessions
land there. The web tests go through real HTTP and WebSocket connections.

## Map

| file | what it holds |
|---|---|
| `main.go` | flags, startup, the terminal |
| `reload.go` | `/reload` and SIGUSR1: build, wait for the chat to be idle, stop every call, exec in place |
| `chat.go`, `view.go` | the log, the summary tree, the view and its merges |
| `claude.go` | `claude -p` calls: turns, compactions, subagents |
| `agents.go`, `subs.go`, `resume.go` | subagents, the page's agents rail, resuming saved sessions |
| `mcp.go`, `wake.go` | the `zoom`, `date`, `search`, `spawn`, `tell` and `wake` tools over MCP; wake-ups |
| `hub.go`, `web.go` | updates to pages and the terminal; the page's server and its guards |
| `find.go` | the page's find (Ctrl/Cmd+F): matches in the whole log, and the log around one |
| `tree.go` | the page's view mode (the view, line by line, and its zooms) and `/tree`, the whole memory |
| `attach.go`, `mention.go` | attachments; the `@` path menu |
| `import.go` | importing past chats |
| `ccconfig.go`, `control.go`, `perms.go`, `limits.go` | the user's Claude Code setup, Claude Code's own lists, permission modes, usage limits |
| `limitresume.go` | a usage limit's reset time, read from its message; the resume at the reset, kept in `<dir>/resume.json` |
| `prompt.md` | the system prompt |
| `web/` | the page: Preact and htm, no build step; `web/vendor/` is vendored as is |

## Rules

- The main chat never compacts, by any setting or means: the view is its
  memory. Claude Code's own auto-compaction stays off in every call but
  subagents'. A subagent compacts past its window (`-agent-compact-max`,
  or less if the user or Opt asks), between two steps, never in one.
- New behavior is on by default, with a flag or setting to turn it off.
- Only subagent calls keep a Claude Code session on disk
  (`--session-id`/`--resume`); turns, compactions and control calls run
  with `--no-session-persistence`.
- The page is light-theme only.
- Never write the user's Claude Code settings (`~/.claude/settings*.json`);
  opt-chat's own choices go in `<dir>/settings.json`.
- To try a build against a real chat, copy the chat directory, delete
  `web.json` and `lock` in the copy, and run it on another port with
  `-open=false -dir <copy>`. Never point a test build at a live chat.
- The branch is `master`.

## Style

Comments, docs and commit messages are plain English in the voice of the
code around them: short sentences that say what a thing is or does, no
marketing words. Keep README.md under about 100 lines; longer prose goes
in the guide or a dated doc.
