# Working on opt-chat

Read [README.md](README.md) first, then the [design notes](docs/2026-10-12-design.md)
for why the code does what it does, and the [guide](docs/2026-10-12-guide.md)
for what the user sees. Dated docs in `docs/` record decisions as of their
date; add a new dated file rather than rewriting history.

## This repo is public

Everything committed here, history included, is on public GitHub and stays
there: a commit can't be taken back without rewriting history. Never commit:

- anything from a real chat: messages, summary lines, imported sessions,
  eval questions, gold answers or model answers, screenshots of the page,
  project, subagent or people's names taken from it, measurements of it;
- personal data: emails, phone numbers, home paths with a user name,
  host names, account ids, facts about the user;
- secrets: API keys, tokens, cookies, the page's `?t=` key;
- sound files or sound pack names: sounds come from the user's own
  peon-ping install (`sound.go`).

Eval data and anything else drawn from a chat go in `private/`, which git
ignores. Test fixtures are made up (`/Users/me`, `me@example.com`, subagents
named `Lead` or `Idle`).

Before a push:

1. `gitleaks git .` finds nothing.
2. Read the whole diff to be pushed, and its commit messages, for the
   things above; `git diff origin/master..master` shows it. gitleaks finds
   secrets, not a project's name or a home path.

## Build and test

```sh
go build -o opt-chat .
go test ./...          # a few minutes
go test -short ./...   # skips the fsync-heavy tests
go test -race ./...    # before a push
gofmt -l .             # must print nothing
go vet .
node --check web/app.js
```

The tests never call the real `claude`: the test binary plays it, speaking
stream-json (see `fakeChat`, `fakeAgent` and `TestMain` in the tests).
`fakeAgent` returns each call's arguments; `fakeFiles(a, ".ctl")` lists the
control requests the calls got (`.env` their environment). `TestMain`
points `CLAUDE_CONFIG_DIR` at a temp dir, so subagent sessions land there.
The web tests go through real HTTP and WebSocket connections.

The binary embeds `web/` and `prompt.md` (`go:embed`): rebuild after
changing them, or a running build serves the old page. To see the page,
run a build on a scratch folder and another port, and open it in a browser:

```sh
go build -o /tmp/oc . && /tmp/oc -dir /tmp/ocdir -open=false -addr 127.0.0.1:8977 -sounds=false
```

It prints the page's address with its key. A subagent of your own (Alt+N
on the page) is a real `claude` call, so keep tries short.

## Map

| file | what it holds |
|---|---|
| `main.go` | flags, startup, the terminal, the commands (`/model`, `/effort`, `/help`, ...) |
| `reload.go` | `/reload` and SIGUSR1: build, wait for the chat to be idle, stop every call, exec in place |
| `chat.go`, `view.go` | the log, the summary tree, the view and its merges |
| `claude.go` | `claude -p` calls: turns, compactions, subagents |
| `turnend.go` | a turn that stops short says why in the chat; a cut-off turn resumes at the next start (`turn.json`) |
| `agents.go`, `subs.go`, `resume.go`, `spawns.go` | subagents, the page's agents rail, resuming saved sessions; subagents a subagent starts |
| `loop.go` | a subagent kept at work turn after turn by a Stop hook opt-chat runs itself |
| `agentmem.go` | subagents' memory: each one's chat summarized into a tree of its own, `zoom("Name")` and `zoom("Name:id", n)` |
| `agentrecall.go`, `agentsearch.go` | recall into a turn's input and `search`, over the chat and the subagents' steps |
| `mcp.go`, `wake.go` | the `zoom`, `date`, `search`, `spawn`, `tell` and `wake` tools over MCP; wake-ups |
| `optmcp.go` | Opt's own MCP servers, `mcp.json` in the chat's folder, apart from the user's |
| `hub.go`, `web.go` | updates to pages and the terminal; the page's server and its guards; the app's manifest |
| `notify.go`, `sound.go` | each kind of event's sound and desktop notification, by the user's switches; playing a sound |
| `btw.go`, `eval.go` | `/btw` side questions, kept in `btw/`; `-eval`, questions asked of a copy of the chat to measure its memory |
| `status.go`, `usage.go` | `/status`; `/usage`, the plan's limits and what opt-chat's calls spent (`usage/`) |
| `find.go` | the page's find (Ctrl/Cmd+F): matches in the whole log, and the log around one |
| `tree.go` | the page's view mode (the view, line by line, and its zooms) and `/tree`, the whole memory |
| `attach.go`, `mention.go`, `voice.go` | attachments; the `@` path menu; voice messages, transcribed on Groq when `GROQ_API_KEY` is set |
| `import.go` | importing past chats |
| `email.go` | keeps the account email Claude Code adds to each call out of the chat and its summaries |
| `ccconfig.go`, `control.go`, `perms.go`, `limits.go` | the user's Claude Code setup, Claude Code's control requests and lists, permission modes, usage limits |
| `limitresume.go` | a usage limit's reset time, read from its message; the resume at the reset, kept in `<dir>/resume.json` |
| `formal/agentmem/` | a TLA+ model of subagents' memory: feeds, zooms, the pump, closing, crashes; the `agent-memory` skill says when and how to run it |
| `formal/looptell/` | a TLA+ model of tells to a subagent in a loop: replays, results, the Stop hook, rest; the `loop-tells` skill says when and how to run it |
| `prompt.md` | the system prompt |
| `web/` | the page: Preact and htm in `app.js`, no build step; `web/vendor/` is vendored as is |
| `docs/` | the guide, the design notes, and dated decisions; `*-borrowed*.md` list ideas taken from other projects, each with its source |

## Code

- JSON is `encoding/json/v2`: field names match case-sensitively, so a
  struct read from JSON needs its tags, tests' too.
- `Agent.mu` guards the subagents' maps (`running`, `resting`, `halts`,
  `subs`). Keep another subagent's disk I/O and locks out of it: the
  `agent-memory` model checks that no step waits on another subagent's
  I/O. Run the `agent-memory` or `loop-tells` skill when a change touches
  what they say they cover.
- A subagent's record (`agents/<Name>.json`) changes through `Agent.note`,
  one field at a time; `runAgent` writes back the whole record it read
  once, before the call starts, so a change to a subagent still starting
  waits or is refused.
- What Claude Code does, as measured, is noted where the code relies on it,
  with its version ("measured on 2.1.322"). Check it again on a new
  Claude Code before trusting it. `strings` on the `claude` binary lists
  the control requests it takes (`subtype:"set_model"`, ...).
- A fix comes with a test that failed before it, for the right reason.

## Rules

- The main chat never compacts, by any setting or means: the view is its
  memory. Claude Code's own auto-compaction stays off in every call but
  subagents'. A subagent compacts past its window (`-agent-compact-max`,
  or less if the user or Opt asks), between two steps, never in one; one
  the user makes from the page has the window they chose, or none.
- New behavior is on by default, with a flag or setting to turn it off.
- Only subagent calls keep a Claude Code session on disk
  (`--session-id`/`--resume`); turns, compactions and control calls run
  with `--no-session-persistence`.
- The page is light-theme only. Its animations run even with the system's
  Reduce Motion on: they show what is at work.
- The page writes the clipboard only when the user asks (a copy button,
  a copy).
- Never write the user's Claude Code settings (`~/.claude/settings*.json`)
  or `~/.claude.json`; opt-chat's own choices go in `<dir>/settings.json`.
- To try a build against a real chat, copy the chat directory, delete
  `web.json` and `lock` in the copy, and run it on another port with
  `-open=false -dir <copy>`. Never point a test build at a live chat.
- The branch is `master`. Never force-push.

## Commits

Small commits, each one change that builds and passes its tests. The
subject says what changed, often as `Area: what it does now` (`Rail: a tab on
the right edge brings back the rail hidden by its ×`); the body says why,
and names the source of a borrowed idea with its URL. A commit an agent
wrote ends with its `Co-Authored-By:` line.

## Style

Comments, docs and commit messages are plain English in the voice of the
code around them: short sentences that say what a thing is or does, no
marketing words. Keep README.md under about 100 lines; longer prose goes
in the guide or a dated doc. A user-visible change updates the guide in
the same commit.
