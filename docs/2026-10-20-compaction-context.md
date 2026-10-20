# A compaction's context, and imports as a side of their own

Written 2026-10-20, after an import cost $281 in one day.

## What went wrong

A compaction's context was every line of the compaction view up to its
node. The view counts a line not built yet as nothing, so it never merged
them away; a compaction showed each as its message's head. During an
import, short messages are built at once, for free, so their merges were
ready far ahead of the messages not built yet, and merges went first. At
the start of that import, 286 merges were ready, with contexts of 369 KB
at the median, 824 KB at p90 and 902 KB at most, against the compaction
view's 16 to 32 KB.

## What changed

Following [UniiChat](https://github.com/VictorTaelin/UniiChat/blob/main/Memory.ts)
(`build`, `starts`, `side`, `settle`):

- **The compaction view ends at the first message not summarized yet**, of
  either side, and holds built lines alone, so its size is what a
  compaction reads. When its batch waits for parents it stops growing
  rather than pass 32 KB. A compaction's context is that view up to its
  node: the compactions running at once all open with the same lines.
  Those lines go in the cached first message, at a multiple of 8 lines,
  not 64: the view is a few dozen lines, and it changes as the frontier
  moves.
- **Past that message, the view's lines up to the node follow**, newest
  first, while the context stays within 32 KB: lines built, and a message
  being summarized alongside as its head. They differ from one compaction
  to the next, so they come after the cached part. This keeps the old
  design note (a message compacted in parallel shows its head), now only
  within budget, and it gives the chat's own compactions the chat's latest
  messages as context during an import. It costs about 4 points of cache
  reads in the replay test.
- **A merge far ahead waits.** Of a side, a node starts only when its
  context ends at most `-jobs` messages not summarized into that side, the
  earliest context first, a merge first at a tie. Built first, as before,
  a merge far ahead could only have a context with a hole or with
  placeholders.
- **An import is a side of its own.** Imported messages are marked
  (`imported` in the log). The chat's nodes start before an import's, and
  a call waits only for the chat's backlog, never the import's.
- **`-jobs`** sets the compactions running at once (default 8), the chat's
  first. (After [pi-optchat #190](https://github.com/jonaslsaa/pi-optchat/pull/190):
  a provider may cap parallel requests.)
- **A batch waits for the most due pair's parent** instead of merging a
  newer pair in its place. During an import the import's parents come late,
  and the batch merged the chat's latest lines meanwhile. A view over
  128 KB whose batch waits shows to calls as `callView` renders a backlog:
  its most due pairs as their parents, down to 64 KB, so the oldest go
  first and the chat's latest exchanges last.
- **The first message of a call never reaches past the first line not
  summarized yet**: once built it reads otherwise, and the first message
  with it.
- **An imported chat's AI is kind `ai`**, not `opt`: its replies are not
  Opt's. Logs from before keep `opt`.
- A leader's ack releases the calls waiting on the same first message as
  its response begins, not when it ends (UniiChat's `AskAI.ts` single
  flight). Free lines are written in one write per message logged or line
  built, not one sync each. An unreadable `view.json` is kept as
  `view.json.bad-<unix time>`.

## Measured

On the state at that import's start: the 286 merges ready now have
contexts of 31.8 KB at the median and 32.0 KB at most (each would start
only when near), and the 8 that start first 16.6 to 21.3 KB.

`TestCacheReplay` replays 4,000 messages, 1,500 of them an import in the
middle with the chat going on, through the chat's own scheduling and views
against a fake cache with Claude Code's marks (no lookback, which never
read back through Claude Code). Before: compactions read 37% of their
input from the cache, one saw 527 KB of context, and 42 turns saw the
latest reply merged into a line with others. After: none saw over 32 KB,
and every turn sees the latest reply as its own line. Compactions read
83% at first; since the compactions' view batches on its own (a later
fix: it had frozen at 32 KB until the chat's view batched, and context
ended far before the node) and calls skip the ack when their first
message is cached (below), they read 72%, at $102 against $124 for an ack
every time. Turns read about 79%. UniiChat, with lookback, asks 95% of
both.

## The ack, priced

A compaction sends its view's first part alone, waits for the model's
"ok" (the ack), then sends the rest with its task. On a copy of the chat
during that import, 117 Sonnet results cost $1.81: 63 lines at $0.0287
each, and 54 acks made 65% of it. 26 of the acks wrote 7k to 15k tokens
anew, because the compactions' view changed with nearly every line built,
and most first messages were used by one or two calls.

What Claude Code's caching allows, probed with `claude -p` in stream-json
on Haiku and Sonnet 5.5 (Claude Code 2.1), reading each result's
`cache_creation_input_tokens` and `cache_read_input_tokens`:

- An entry is written only where Claude Code marks: the system prompt and
  the end of each of the last two messages. A first message sent alone is
  marked at its end.
- A mark reads back a prefix cached at the end of an earlier block, in
  its own message or before it. One message of three blocks (a first
  message cached by another call's ack, the rest of the view, the task)
  read the whole first message back, and Sonnet answered the task, not
  "ok".
- One message never writes an entry at a block inside it: the same three
  blocks sent twice with nothing cached read nothing back.
- Two user messages sent at once run as two turns, so they give no mark
  for free. Two acks in a row, a cached first part and then a second, are
  not shared between calls when the model thinks: its "ok" differs.

`TestCacheReplay`'s fake now marks block by block, with the API's lookback
of 20 blocks, and prices requests (writes 1.25 times input, reads 0.1, the
system prompt read by every request, 4 tokens out for an ack). Cost of
each way, against acking every call at 8-line checkpoints, in the replay
and in a replay of that chat's import from a copy:

| way | replay | the chat's import |
|---|---|---|
| no ack: the whole view in one message | 1.84 | 1.13 to 1.32 |
| ack every call, checkpoints of 4, 16, 32, 64 lines | 1.02, 1.05, 1.16, 1.45 | 16: 0.93 |
| first message fixed until the compactions' view batches | 1.20 | |
| one call acks; the others send its first message with the rest | 0.83 | 0.92 to 0.95 |
| as above, also reusing a cached first message 8 lines shorter | 0.84 | 0.81 to 0.83 |
| as above, 16 lines shorter | 0.87 | 0.79 to 0.80 |

What shipped is the fifth row (`compactView`): a compaction whose first
message, or one a checkpoint shorter, was written or read in the last 4
minutes (Claude Code's entries live 5), or is being written by a call
whose response has begun, sends it as the first block of one message with
the rest; any other acks it as before. Turns are unchanged.

Live, from the same copy for 50 seconds: 61 lines at $0.0170 each, acks
included, against $0.0287; 17 acks against 54 for 63 lines; no compaction
answered "ok"; no first message served more than 6 calls. With 16 lines
of reuse, a run of 75 seconds gave $0.0218, one stretch of 13 calls each
writing 4k to 6k tokens behind a short first message.
