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
  result under it (`Read 12 lines`, `Updated with 2 additions and 1
  removal` for files); click it, or Enter on it, for the whole input and
  output. A spinner tells what Opt is doing, for how long and how many
  tokens it has written so far; click its `esc to interrupt` to stop.
- The input stays put at the bottom: Enter sends, Shift+Enter starts a new
  line, Up and Down go through your earlier messages. Output never touches
  what you type.
- While Opt works, Enter queues a message. It shows under the spinner,
  where you can edit it, take it out, or send it now (⌘/Ctrl+Enter), which
  stops the turn and sends the queue at once. Otherwise it reaches Opt as
  soon as Opt could read it: at once while a tool call runs, with that
  call's result; else at the next tool call; else it starts the next turn.
  Up on an empty input takes the last queued message back to edit.
- Esc stops the turn, keeping what Opt had written, marked Interrupted
  (`[interrupted]` in the log); your queued messages come back into the
  input, as in Claude Code.
- Attach files by dropping them on the page (a file from Finder, or the
  thumbnail ⌘⇧4 leaves in the corner), by pasting with ⌘V or Ctrl+V
  anywhere on the page (a screenshot copied with ⌃⌘⇧4, an image copied
  from a page, a file copied in Finder), with 📎, or by typing a path,
  `/…` or `~/…`, which shows the file it attaches above the input (×
  leaves it as text). A hidden file or folder (`~/.zshrc`, `~/.ssh/…`) is
  only attached once you press its +, and a path in a block of code, or
  in a message naming more than ten, is just text. Images and PDFs go to
  the model as such; text files are inlined; any other file goes as a path
  its tools can read. See [Attachments](#attachments).
- `@` and part of a path opens a menu of the files and folders it
  completes to, as in Claude Code: a folder's entries (`@~/dev/`, `@../`,
  `@src/ap`, relative to `-cwd`), and for a name alone also the files below
  `-cwd` that have it (a quarter of a second's search). ↑/↓ choose, Tab or
  Enter takes one: a folder opens on its own entries, a file goes in as
  `@~/path` (a space in it escaped, `My\ File.png`), which attaches it as a
  typed path does. Esc closes the menu.
- A long message of yours shows folded, as a big paste does in Claude Code.
- Scroll up for the whole chat, back to its first message, loaded as you go.
  A reload, a second tab or another window shows the turn underway,
  mid-reply; a page that loses opt-chat reconnects by itself, also after a
  restart.
- Alt+P (or `/model`, or the model in the bar) opens the model picker, as
  in Claude Code: Claude Code's own list of models, ↑/↓ or a number to
  pick, ←/→ for the effort. Enter keeps the choice for later runs too (in
  `<dir>/settings.json`; a `-model` or `-effort` flag wins over it), `s`
  for this run only, Esc cancels. `/model opus` and `/effort high` switch
  them until you quit. `/agents` lists the subagents at work, `/help` the commands.
- `/name` runs one of your skills, as in Claude Code: the line goes to Opt
  and Claude Code runs the skill, with what follows the name as its
  arguments. Typing `/` lists the commands and your skills (a plugin's by
  their own name too): ↑/↓ choose, Tab completes, Enter runs the chosen
  one, Esc shuts the list. A line starting with any other `/word`, or with
  a skill turned off in `/skills`, is not sent: it comes back to the input.
- Shift+Tab cycles the permission mode, as in Claude Code: manual (only
  reads go ahead unasked), accept edits (file edits too), auto (whatever
  Claude Code's classifier deems safe). The switch reaches the turn and
  subagents underway. A call Claude Code asks about shows its command or
  change and waits, with the keys, unless you are typing (then Tab or a
  click goes to it, so a key meant for your message never answers it):
  1 or Enter says yes, 2 yes and don't ask again (for this run, its later
  calls too; nothing is written to your Claude Code settings), 3 or Esc
  no, which stops the turn so you can say what to do instead. A subagent's
  questions show its name, and a no lets it go on. In the terminal (-repl),
  the next line answers: y, a, n, or anything else for no with that line
  sent to Opt as what to do instead.
- The bar at the bottom shows the memory: messages, the view's size, what is
  left to summarize, and the last turn's tokens. `?` on an empty input lists
  the shortcuts. While a reply lands in a tab in the background, its title
  says so.

Put your own instructions in `<dir>/AGENTS.md`: who you are, how your files
are organized, how you want work done. They follow the system prompt. Edit
the file only when you must, since the whole prompt cache starts over after
a change.

| flag | default | |
|---|---|---|
| `-model` | `fable` | model for turns |
| `-effort` | `high` | effort for turns: `low`, `medium`, `high`, `xhigh` or `max` |
| `-agent-model` | `opus` | model for subagents |
| `-agent-effort` | `high` | effort for subagents |
| `-compact-model` | `haiku` | model for compactions |
| `-compact-effort` | `high` | effort for compactions (the recipe's: `xhigh`) |
| `-permission-mode` | `auto` | permission mode to start in: `manual`, `acceptEdits` or `auto` |
| `-tools` | `Bash,Read,Edit,Write,WebSearch,WebFetch` | Claude Code tools (with your setup, also `Skill` and `ToolSearch`) |
| `-claude-config` | on | your Claude Code skills, plugins, MCP servers and settings in turns and subagents (below) |
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
another site's page are refused, the page runs only its own scripts and
styles, and a reply's HTML is sanitized before it shows, so it can't run
code or restyle the page. Files you attached are served
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
more often tried a tool or wrote about itself in a line. So compactions run
at `high`: about 3x faster than at `xhigh`, on a quarter of the usage
(`-compact-effort xhigh` for the recipe's).

## Files

```
<dir>/
  main/YYYY-MM-DD.jsonl   messages: {i, kind, text, size, date, images, ref, part}, append-only, fsynced
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
  screenshot stays a PNG, a photo becomes a JPEG, turned upright as its
  EXIF orientation says. Only the image as sent is kept. HEIC photos, as
  iPhones and macOS take them, become JPEGs (with macOS's `sips`). An image
  that would take over 400 MB decoded (100 megapixels; 50 at 16 bits) or a
  file over 200 MB is a path, never decoded.
- PDFs up to 20 MB go to the model as documents (a request holds 32 MB,
  and base64 adds a third); a bigger one is a path.
- Text files are inlined in the mark, up to 25,000 characters in one
  message in all; a longer one is a path, which Read opens.
- Any other file is a path the model's tools can read.

An attachment has a place in your message, as in Claude Code: attaching
one (paste, drop or 📎) types `[Image #1]` (`[PDF #2]`, `[File #3]`) where
the caret is, and deleting that takes the attachment off. "Do this
[Image #1], not this [Image #2]" reaches the model as written: each mark
where its placeholder was, each image right after its mark. A file named by
a path you type gets its mark after the path. An attachment with no place
(its placeholder deleted in a queued message's edit) goes at the end. The
page shows each image where you put it.

A message's images and PDFs are part of it. Yours go with your message; a
tool result's images, such as an image file that Read opened, go with its
`echo`. The call sees each after its mark (a tool result's after the
text), and so does the compaction that writes the message's line, which
says in words what they show: such a message is never its own line.
`zoom(id, 1)` gives the message whole, its images in their places (a PDF
there is a path).

## Your Claude Code setup

Turns and subagents run with your own Claude Code skills, plugins, MCP
servers (yours, your plugins' and claude.ai's connectors) and settings, as
`claude` run in `-cwd` has them, but never your hooks, CLAUDE.md or
Claude Code's memory, which would make Opt someone else. `-claude-config=false`
runs them bare, as compactions always run.

- `/mcp` lists the MCP servers with their status, live as they connect:
  Enter or Space turns one off or on for Opt (kept in `<dir>/settings.json`:
  Claude Code saves no such choice for `claude -p`), `a` signs in to one
  that needs it (the sign-in is Claude Code's own, for every session), `r`
  reconnects one.
- `/skills` lists the skills, each with what its listing costs every turn:
  Enter or Space turns one on or off for Opt, from its next call (kept in
  `<dir>/settings.json` and given to each call as settings; your Claude
  Code keeps its own). A skill your organization or a plugin manages stays
  as it set it.
- `/plugins` lists your plugins, each with the skills, MCP servers and
  tokens it brings: Enter or Space turns one on or off for Opt, all of it
  at once (turn off `figma` and its skills and MCP server go), from its
  next call, kept in `<dir>/settings.json` and given to each call as
  settings (`enabledPlugins`); your Claude Code keeps its own.
- In the terminal, `/mcp`, `/skills` and `/plugins` list them; `/mcp off
  NAME`, `/skills on NAME`, `/plugins off figma@claude-plugins-official`
  and so on switch one.

What it costs: the skills and servers sit in a block Claude Code adds after
the view's first message (with the default setup here, about 20k tokens),
cached with it. Two things keep it cached from turn to turn. Before a call
sends the view it waits, up to 5 seconds, for the MCP servers that were
connected last time to connect again, since the block lists those connected
by then (a server that is always slow is not waited for). And calls run
with `ENABLE_TOOL_SEARCH=true` (MCP tools load when needed, so a server's
hundreds of tools never sit in every prompt) and `CLAUDE_CODE_TETHER_LIVE=0`.
With Claude Code's server-side threads, each request after the first sent
only what was new, and the server wrote the whole block to the cache again
on every turn. Measured on a 27,000-message chat: about 22k tokens written
per turn with threads, a few hundred without.

## Subagents

`spawn(name, task, effort, model, autocompact)` starts a subagent in the
background and returns at once; `effort`, `model` and `autocompact` are
optional (`-agent-effort` and `-agent-model`, Opus, when left out; ask Opt
for "a Fable subagent" and it passes `fable`). A subagent is
a fresh call with the same prompt and tools, which gets the view, then its
task. Its steps go to `agents/<Name>.log`, which `zoom("Name")` opens whole
(Claude Code saves a long one to a file the model reads). Its final reply
comes back as a `work` message, `[Name] report`, ending with the model it
ran on and `Full chat: zoom("Name")`. If a turn is running, the report is injected into
it; otherwise it starts a new turn. The chat never waits for a subagent.
`tell(name, message)` reaches a running subagent between its tool calls.
Subagents can't start or tell subagents. Quitting stops them, and each one's
report then says it was stopped.

A long task can outgrow a subagent's context. `/autocompact 250k` (saved in
`<dir>/settings.json`; `/autocompact off` undoes it) lets Claude Code compact
a subagent's context at 250k tokens, and `spawn`'s `autocompact` sets it for
one subagent. After a compaction the subagent gets the view and its task
again, as they are then, and goes on. It is off unless set, whatever your
Claude Code's own setting. The chat's own calls never compact, whatever is
set: the view is its memory, and it never outgrows its size.

## Where it departs from the recipe, and why

The recipe assumes direct API calls. Running through `claude -p` changed four
things, each measured through a logging proxy; testing changed two more, and
use three more:

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
3. **A failed compaction** is tried again at your next message, as the recipe
   says, and on a timer, 10 s at first, doubled at each failure up to 10
   minutes, so a usage limit or an outage is not hammered. Meanwhile the
   lines after it get built, and calls don't wait for it. A line that keeps
   failing while other lines get built (the 5th such failure; a message with
   images is tried without them from the 3rd) is its source cut to size,
   ending "(not summarized: zoom it)": one bad message never holds up the
   tree above it. Compactions see a message not summarized yet as its head,
   not as the end of their context.
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
7. **A call waits for summaries only when that is short**: when at most 32
   lines are left to build (four rounds of 8 compactions), and for at most a
   minute. Behind a bigger
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
9. **Compactions run at effort `high`**, not the recipe's `xhigh`: their
   lines read as well, about 3x faster, on a quarter of the usage (see
   Importing past chats).

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
go test ./...          # ~70 s: three tests fsync thousands of writes
go test -short ./...   # skips them: ~20 s
```

The tests run turns, compactions and subagents against a fake `claude`: the
test binary itself, speaking stream-json. The web server's tests go through
real HTTP and WebSocket connections: its key and guards, uploads, the
socket's requests and the queue. The page itself was checked in a real
browser, against real Claude.
