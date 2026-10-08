# opt-chat

One chat with Claude that never ends, after Taelin's
[UniiChat recipe](https://gist.github.com/VictorTaelin/91837951a5ce5b38f341ec1ba1df6449).
The chat is the memory: every message goes into an append-only log, a cheap
model compresses the log into a binary tree of one-line summaries, and each
turn starts fresh from a constant-size **view** of the whole chat. The model
reads recent messages one line each and older ones coarser, and **zooms** into
any line, down to the message itself, when it needs more detail.

Claude Code is the tool loop. Every turn, agent and compaction is a
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
- A message sent while a turn runs reaches it between tool calls.
- Ctrl-C stops the turn, or the wait before it. Ctrl-C when idle, or Ctrl-D, quits.
- Put your own instructions in `<dir>/AGENTS.md`: who you are, how your files
  are organized, how you want work done. They follow the system prompt. Edit
  the file only when you must, since the whole prompt cache starts over after
  a change.

| flag | default | |
|---|---|---|
| `-model` | `opus` | model for turns and agents |
| `-effort` | model default | effort for turns and agents |
| `-compact-model` | `haiku` | model for compactions |
| `-compact-effort` | `xhigh` | effort for compactions |
| `-permission-mode` | `auto` | Claude Code permission mode for turns and agents |
| `-tools` | `Bash,Read,Edit,Write,WebSearch,WebFetch` | Claude Code tools for turns and agents |

After each turn a dim line shows cache use for the view, for the turn, and
for the compactions run since the last turn.

## Files

```
<dir>/
  main/YYYY-MM-DD.jsonl   messages: {i, kind, text, size, date}, append-only, fsynced
  tree/YYYY-MM-DD.jsonl   summary nodes: {l, i, text, size}, append-only, fsynced
  view.json               the view, as [l, i] pairs, kept across restarts
  agents/<Name>.log       each agent's own steps
  AGENTS.md               your instructions (optional)
  lock                    one process per chat
```

Back the directory up. Never edit the logs: they are the chat's history.

## Agents

`spawn(name, task)` starts an agent in the background and returns at once. An
agent is a fresh call that gets the view, then its task. Its steps go to
`agents/<Name>.log`, which `zoom("Name")` opens. Its final reply comes back as
a `work` message, `[Name] report`. If a turn is running, the report is
injected into it; otherwise it starts a new turn. The chat never waits for an
agent. `tell(name, message)` reaches a running agent between its tool calls.
Agents run as long as opt-chat does: quitting stops them, and their reports
are lost.

## Where it departs from the recipe, and why

The recipe assumes direct API calls. Running through `claude -p` changed four
things, each measured through a logging proxy:

1. **How the view is cached.** The recipe splits the view into blocks of 4
   lines, marks the last whole block, and relies on the API's 20-block
   lookback to find the previous call's entry. Claude Code places its own
   marks: on the last two messages, plus, on Opus and Sonnet, on an
   environment block that it pins after the first message. Growing a marked
   message never read back from the cache on any model tried. Exact repeats
   always did. So the view's first message holds its lines up to the last
   multiple of 64, which stays byte-identical until 64 more lines arrive or a
   batch merges. The model answers it "ok". The rest of the view and the
   input follow in the second message. Each call reads the first message from
   the cache and writes only the lines after it.
2. **Compactions** run on another model than turns, so they could never read
   the turns' cache. They get the same system prompt but no tools.
3. **A failed compaction** is retried every 10 s, not only at the next
   message, so a waiting turn never stalls until you type again.
4. **The child environment** drops the variables of any Claude Code session
   that launched opt-chat. With them inherited, that session's token-budget
   reminder appeared in some calls and not others, and no two prompts
   matched.

Everything else follows the recipe: the log and its kinds, the free nodes,
512-byte lines, the merge order (`TestMostDueMatchesPush` checks it against
Taelin's rollback push over 20,001 steps), the 128 KB → 64 KB sawtooth, the
16–32 KB compaction view, the compaction tasks with ruler, cut and five tries,
8 parallel compactions, ready-node queues, and waiting for summaries before
every call.

The recipe's computer-use tools and image support are not built.

## Test

```sh
go test ./...          # ~15 s: the sawtooth test fsyncs ~2,000 writes
go test -short ./...   # skips it
```
