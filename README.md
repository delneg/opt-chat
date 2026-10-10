# opt-chat

One chat with Claude that never ends, after Taelin's
[UniiChat recipe](https://gist.github.com/VictorTaelin/91837951a5ce5b38f341ec1ba1df6449).
The chat is the memory: every message goes into an append-only log, a cheap
model compresses the log into a binary tree of one-line summaries, and each
turn starts fresh from a constant-size **view** of the whole chat. The model
reads recent messages one line each and older ones coarser, and **zooms** into
any line, down to the message itself, when it needs more detail.

Claude Code is the tool loop. Every turn, subagent and compaction is a
`claude -p` call on your Claude subscription. opt-chat owns the memory (the
log, the tree, the view, and the `zoom`/`date`/`spawn`/`tell` tools, served
to those calls over MCP) and the chat's page, which you use in your browser,
much as you use Claude Code in a terminal.

## Run

Requires Go 1.27 and a logged-in `claude` CLI.

```sh
go build -o opt-chat .
./opt-chat                       # chat in ~/.opt-chat, tools run in ~
./opt-chat -dir ~/chats/work -cwd ~/dev
./opt-chat -import ~/.claude      # import your past chats, then chat
```

`./opt-chat` prints the chat's address and opens it in your browser. The
terminal then shows only how the summaries come along and any warnings;
Ctrl-C there quits, stopping every call and subagent underway and logging
what was waiting. `-repl` chats in the terminal instead, as plain lines,
which is handy to script.

The page works like Claude Code:

- The reply streams in as it is written, as Markdown with highlighted code.
  Each tool call is one line, `Bash(ls -la)`, with the first lines of its
  result under it; click it for the whole input and output. A spinner tells
  what Opt is doing, for how long and how many tokens it has written so far.
- The input stays put at the bottom: Enter sends, Shift+Enter starts a new
  line, Up and Down go through your earlier messages. Output never touches
  what you type.
- While Opt works, Enter queues a message. It shows under the spinner,
  where you can edit it, take it out, or send it now (⌘/Ctrl+Enter), which
  stops the turn and sends the queue at once. Otherwise it reaches Opt as
  soon as Opt could read it: at once while a tool call runs, with that
  call's result; else at the next tool call; else it starts the next turn.
  Up on an empty input takes the last queued message back to edit.
- Esc stops the turn, keeping what Opt had written, marked
  `[interrupted]`; your queued messages come back into the input, as in
  Claude Code.
- Attach files by dropping them on the page (a file from Finder, or the
  thumbnail ⌘⇧4 leaves in the corner), by pasting (⌘V of an image), with
  📎, or by typing a path, `/…` or `~/…`, which shows the file it attaches
  above the input (× leaves it as text). Images and PDFs go to the model
  as such; text files are inlined; any other file goes as a path its tools
  can read. See [Attachments](#attachments).
- Scroll up for the whole chat, back to its first message, loaded as you go.
  A reload, a second tab or another window shows the turn underway,
  mid-reply; a page that loses opt-chat reconnects by itself, also after a
  restart.
- `/model opus` and `/effort high` switch the turns' model and effort until
  you quit (`/effort default` for the model's own); `/model` alone shows
  them. `/agents` lists the subagents at work, `/help` the commands; Tab
  completes them. A line starting with any other `/word` is not sent.
- The bar at the bottom shows the memory: messages, the view's size, what is
  left to summarize, and the last turn's tokens. `?` lists the shortcuts.

Put your own instructions in `<dir>/AGENTS.md`: who you are, how your files
are organized, how you want work done. They follow the system prompt. Edit
the file only when you must, since the whole prompt cache starts over after
a change.

| flag | default | |
|---|---|---|
| `-model` | `fable` | model for turns |
| `-effort` | model default | effort for turns |
| `-agent-model` | `opus` | model for subagents |
| `-agent-effort` | model default | effort for subagents |
| `-compact-model` | `haiku` | model for compactions |
| `-compact-effort` | `xhigh` | effort for compactions |
| `-permission-mode` | `auto` | Claude Code permission mode for turns and subagents |
| `-tools` | `Bash,Read,Edit,Write,WebSearch,WebFetch` | Claude Code tools |
| `-import` | | import past chats first (below) |
| `-import-since` | | import only messages from this day on, `YYYY-MM-DD` |
| `-import-tools` | off | import tool calls and results too |
| `-import-skip` | | skip sessions whose directory (or chat title) contains any of these, comma-separated |
| `-addr` | last run's port, else any | where the page is served, `127.0.0.1:port` |
| `-open` | `true` | open the page in the browser |
| `-repl` | off | chat in the terminal instead |

In `-repl`, type a message and press Enter; a paste of many lines stays one
message, and a path you type attaches its file. Ctrl-C stops the turn, or
the wait before it, and prints the queued messages it held back; Ctrl-C when
idle, or Ctrl-D, quits. After each turn a dim line shows cache use for the
view, for the turn, and for the compactions run since the last turn.

### Who can reach the page

Only this machine: opt-chat listens on 127.0.0.1, and answers only requests
addressed to `127.0.0.1:port` or `localhost:port` (so a web page can't reach
it through a DNS name of its own) that carry the chat's key. The key is in
the address opt-chat prints; opening it once stores the key in a cookie
(HttpOnly, SameSite=Strict) and drops it from the address. Requests from
another site's page are refused, the page runs only its own scripts, and a
reply's HTML is sanitized before it shows. Files you attached are served
only as images or PDFs; anything else downloads, and none of them runs a
script. The key and the port are kept in `<dir>/web.json`, so a page left
open carries on after a restart; delete the file for a new key.

## Importing past chats

`-import` brings chats from before opt-chat into its memory, as UniiChat's
"Import my past AI chats" does:

- `~/.claude` (or a project under it): Claude Code's sessions, and, from its
  `history.jsonl`, your own messages in the sessions whose transcripts are
  gone (Claude Code deletes them after 30 days, unless `cleanupPeriodDays`
  in its `settings.json` says otherwise), without commands, pastes in full;
- `~/.codex`: Codex's sessions;
- a Claude or ChatGPT data export, its `.zip` or the `conversations.json`
  in it;
- one session's `.jsonl`;
- any other file: each line becomes a `note`.

Each session goes into the log oldest first, under a note naming it
(`Claude Code session <id> in ~/dev/x, from 2026-08-01 10:00:00 UTC (imported)`),
each message at its own date, so `date(id)` tells when it was said. The
user's and the assistant's words come in, with their images; tool calls and
results only with `-import-tools`; sessions run in a directory you name in
`-import-skip` (`-import-skip dev/bots,scratch`) not at all; thinking,
Claude Code's own commands and notices, and subagents' sessions never.
Before it writes anything, the import
says how many messages it adds and about how many compaction calls
summarizing them takes, and asks: the log is append-only. Importing again
adds only what is new since, so a session that went on comes in where it left
off, and a line that a forked or resumed session copied comes in once.

Then the chat starts and summarizes the import, 8 compactions at a time, and
you can chat at once: as in UniiChat, a turn answers meanwhile from the tree
as far as it is built (see below), and the spinner says how many messages
it sees unsummarized. The bar at the bottom of the page, and the terminal
each minute, say how many are left and how many lines have been written:
merges of older lines go first, so the count of messages stays put for
hours, and then falls. Quit whenever you like: the next run goes on where
this one stopped.

The time goes to the compaction model's thinking. Building one day of chats
at each effort, Haiku 5.5 thought about 10,400 tokens a line at the recipe's
`xhigh` (41 s a call), 2,600 at `high` (12 s) and 1,100 at `medium` (7 s).
Lines at `high` read as well as at `xhigh`; at `medium` and below the model
more often tried a tool or wrote about itself in a line. So for a big import,
`-compact-effort high` builds about 3x faster, on a quarter of the usage.
Like every flag, it lasts until you quit.

## Files

```
<dir>/
  main/YYYY-MM-DD.jsonl   messages: {i, kind, text, size, date, images, ref}, append-only, fsynced
  tree/YYYY-MM-DD.jsonl   summary nodes: {l, i, text, size}, append-only, fsynced
  view.json               the view, as [l, i] pairs, kept across restarts
  files/<hash>.<ext>      attachments and tool results' images, named by their content
  web.json                the page's key and port
  agents/<Name>.log       each subagent's own steps
  AGENTS.md               your instructions (optional)
  lock                    one process per chat
```

Back the directory up. Never edit the logs: they are the chat's history.

## Attachments

Each file you attach is kept in `files/`, named by its content, and your
message gets a mark for it, which the model and its summaries see:

```
<attachment name="Screenshot 2026-10-10 at 13.48.30.png" path="~/.opt-chat/files/<hash>.png" size="2.6 MB"/>
```

- Images go to the model with the message, at most 2000 pixels a side and
  3.75 MB (5 MB in base64, the API's limit), shrunk when bigger: a Retina
  screenshot stays a PNG, a photo becomes a JPEG. HEIC photos, as iPhones
  and macOS take them, become JPEGs (with macOS's `sips`).
- PDFs up to 30 MB go to the model as documents.
- Text files are inlined in the mark, up to 25,000 characters in one
  message in all; a longer one is a path, which Read opens.
- Any other file is a path the model's tools can read.

A message's images and PDFs are part of it. Yours go with your message; a
tool result's images, such as an image file that Read opened, go with its
`echo`. The call sees them after the text, and so does the compaction that
writes the message's line, which says in words what they show: such a
message is never its own line. `zoom(id, 1)` gives the message whole, with
its images (a PDF there is a path).

## Subagents

`spawn(name, task, effort)` starts a subagent in the background and returns
at once; `effort` is optional (`-agent-effort` when left out). A subagent is
a fresh call with the same prompt and tools, which gets the view, then its
task. Its steps go to `agents/<Name>.log`, which `zoom("Name")` opens whole
(Claude Code saves a long one to a file the model reads). Its final reply
comes back as a `work` message, `[Name] report`, ending with
`Full chat: zoom("Name")`. If a turn is running, the report is injected into
it; otherwise it starts a new turn. The chat never waits for a subagent.
`tell(name, message)` reaches a running subagent between its tool calls.
Subagents can't start or tell subagents. Quitting stops them, and each one's
report then says it was stopped.

## Where it departs from the recipe, and why

The recipe assumes direct API calls. Running through `claude -p` changed four
things, each measured through a logging proxy; testing changed two more, and
use two more:

1. **How the view is cached.** The recipe splits the view into blocks of 4
   lines, marks the last whole block, and relies on the API's 20-block
   lookback to find the previous call's entry. Claude Code places its own
   marks, on the system prompt and on the last two messages, and on Fable,
   Opus and Sonnet pins an environment block after the first message.
   Growing a marked message never read back from the cache on any model
   tried. Exact repeats always did. So the view's first message holds
   its lines up to the last multiple of 64, which stays byte-identical until
   64 more lines arrive or a batch merges. The model answers it "ok". The
   rest of the view and the input follow in the second message. Each call
   reads the first message from the cache and writes only the lines after
   it.
2. **"Never called" is enforced.** A compaction has the turns' system prompt
   and tools, as the recipe says, but Claude Code would run a tool it calls,
   so a `PreToolUse` hook refuses every call. It runs in the `default`
   permission mode: the mode only adds reminders after the system prompt, and
   auto mode, on a model without it, adds a random "Exited Auto Mode: ask
   clarifying questions" mid-call. On one model, turns and compactions read
   each other's tools and system prompt from the cache (measured on Fable).
   The cache is per model, so with the recipe's Haiku they can't.
3. **A failed compaction** is tried again at the next message, as the recipe
   says, and also every 10 s, so a turn waiting on its line never waits for a
   message that won't come.
4. **The child environment** drops the variables of any Claude Code session
   that launched opt-chat. With them inherited, that session's token-budget
   reminder appeared in some calls and not others, and no two prompts
   matched. It also turns off Claude Code's background shells: a call ends
   with its reply, so a command left running in the background died unseen,
   after the model had promised to report its result. Long work goes to
   subagents, as in the recipe.
5. **A batch also goes on as lines get built**, not only at each new
   message, and the view's size counts each line as built. Measured at each
   new message alone, with its newest lines still placeholders, the view
   passed 128 KB unseen once they were built, and after an import of 500
   long notes it reached 203 KB. For the same reason merges build before
   messages: behind a backlog of messages, the parents a batch needs never
   came.
6. **A view.json that can't be read** (a crash mid-write on a system that
   lost it) starts the view over from the log, with a warning, instead of
   keeping the chat shut. It is written whole and synced before it replaces
   the last one, so this should never happen.
7. **A call waits for summaries only when that is short**: when at most 256
   lines are left to build, and for at most two minutes. Behind a bigger
   backlog, as after an import, it goes on at once, as UniiChat does, and
   sees the tree as far as it is built: a message not summarized yet shows
   its head, its first 512 bytes, then `(not summarized yet: zoom it)`, and
   when that makes the view pass 128 KB, runs of such lines, then any lines,
   oldest first, show as their parents, built or not, down to 64 KB. Zoom
   opens those like any other line. With every line built, the view a call
   sees is the view itself, byte for byte, so the cache holds. Waiting for
   every summary, as the recipe does, had a turn after an import wait for
   hours.
8. **Calls run in a process group of their own.** The terminal's Ctrl-C goes
   to its whole foreground group: it killed every call underway with
   opt-chat's, so compactions failed, with an empty error, each time the
   user stopped a turn.

The prompt is the recipe's, with Unii renamed Opt and the paragraph on
computers dropped (opt-chat has no device tools), plus additions: subagent
calls in its opening sentence and in a section of their own, the two-part
view, a line not summarized yet showing its message's head, and the `spawn`
and `tell` tools.

Everything else follows the recipe: the log and its kinds, the free nodes,
512-byte lines, tool output clipped to 30,000 characters, the merge order
(`TestMostDueMatchesPush` checks it against Taelin's rollback push over
20,001 steps), the 128 KB → 64 KB sawtooth, the 16–32 KB compaction view,
the compaction tasks with ruler, cut and five tries, 8 parallel compactions,
and ready-node queues (one pass at start finds the work the last run left).

## From UniiChat itself

Beyond the recipe, a few things follow UniiChat as it ships (its web
client's code): importing past chats, and chatting while they are
summarized; a long text split at a line break when one is in reach;
`spawn`'s `effort`; a report ending with `Full chat: zoom("Name")`; `/model` and `/effort` mid-chat, and a line
starting with an unknown `/word` held back; the chat's own tools logged by
their names (`zoom`, not `mcp__opt__zoom`).

## Test

```sh
go test ./...          # ~30 s: the sawtooth test fsyncs ~2,000 writes
go test -short ./...   # skips it
```

The tests run turns, compactions and subagents against a fake `claude`: the
test binary itself, speaking stream-json. The web server's tests go through
real HTTP and WebSocket connections: its key and guards, uploads, the
socket's requests and the queue. The page itself was checked in a real
browser, against real Claude.
