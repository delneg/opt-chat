# opt-chat

One chat with Claude that never ends, after Taelin's
[UniiChat recipe](https://gist.github.com/VictorTaelin/91837951a5ce5b38f341ec1ba1df6449).
The chat is the memory: every message goes into an append-only log, a cheap
model compresses the log into a binary tree of one-line summaries, and each
turn starts fresh from a constant-size **view** of the whole chat. The model
reads recent messages one line each and older ones coarser, and **zooms** into
any line, down to the message itself, when it needs more detail.

Claude Code is the tool loop. Every turn, subagent and compaction is a
`claude -p` call on your Claude subscription. opt-chat owns only the memory:
the log, the tree, the view, and the `zoom`/`date`/`spawn`/`tell` tools,
served to those calls over MCP.

## Run

Requires Go 1.27 and a logged-in `claude` CLI.

```sh
go build -o opt-chat .
./opt-chat                       # chat in ~/.opt-chat, tools run in ~
./opt-chat -dir ~/chats/work -cwd ~/dev
./opt-chat -import LOG.txt       # each line becomes a `note`, then exit
```

- Type a message and press Enter. A paste of many lines stays one message.
- To send an image, drop its file into the terminal or type its path, as
  `/path/shot.png`, `'/path/my shot.png'` or `/path/my\ shot.png`. PNG,
  JPEG, GIF and WebP go with the message.
- A message sent while a turn runs reaches it between tool calls.
- Ctrl-C stops the turn, or the wait before it. Ctrl-C when idle, or Ctrl-D,
  quits: it stops every call and subagent underway and logs what was waiting.
- Put your own instructions in `<dir>/AGENTS.md`: who you are, how your files
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

After each turn a dim line shows cache use for the view, for the turn, and
for the compactions run since the last turn.

## Files

```
<dir>/
  main/YYYY-MM-DD.jsonl   messages: {i, kind, text, size, date, images}, append-only, fsynced
  tree/YYYY-MM-DD.jsonl   summary nodes: {l, i, text, size}, append-only, fsynced
  view.json               the view, as [l, i] pairs, kept across restarts
  files/<hash>.<ext>      images, named by their content
  agents/<Name>.log       each subagent's own steps
  AGENTS.md               your instructions (optional)
  lock                    one process per chat
```

Back the directory up. Never edit the logs: they are the chat's history.

## Images

A message's images are part of it. Yours go with your message; a tool
result's, such as an image file that Read opened, go with its `echo`. The
call sees them after the text, and so does the compaction that writes the
message's line, which says in words what they show: a message with an image
is never its own line. `zoom(id, 1)` gives the message whole, with its images.

## Subagents

`spawn(name, task)` starts a subagent in the background and returns at once.
A subagent is a fresh call with the same prompt and tools, which gets the
view, then its task. Its steps go to `agents/<Name>.log`, which
`zoom("Name")` opens whole (Claude Code saves a long one to a file the model
reads). Its final reply comes back as a `work` message,
`[Name] report`. If a turn is running, the report is injected into it;
otherwise it starts a new turn. The chat never waits for a subagent.
`tell(name, message)` reaches a running subagent between its tool calls.
Subagents can't start or tell subagents. Quitting stops them, and each one's
report then says it was stopped.

## Where it departs from the recipe, and why

The recipe assumes direct API calls. Running through `claude -p` changed four
things, each measured through a logging proxy, and testing changed two more:

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

The prompt is the recipe's, with Unii renamed Opt and the paragraph on
computers dropped (opt-chat has no device tools), plus additions: subagent
calls in its opening sentence and in a section of their own, the two-part
view, and the `spawn` and `tell` tools.

Everything else follows the recipe: the log and its kinds, the free nodes,
512-byte lines, tool output clipped to 30,000 characters, the merge order
(`TestMostDueMatchesPush` checks it against Taelin's rollback push over
20,001 steps), the 128 KB → 64 KB sawtooth, the 16–32 KB compaction view,
the compaction tasks with ruler, cut and five tries, 8 parallel compactions,
ready-node queues (one pass at start finds the work the last run left), and
waiting for summaries before every call.

## Test

```sh
go test ./...          # ~20 s: the sawtooth test fsyncs ~2,000 writes
go test -short ./...   # skips it
```

The tests run turns, compactions and subagents against a fake `claude`: the
test binary itself, speaking stream-json.
