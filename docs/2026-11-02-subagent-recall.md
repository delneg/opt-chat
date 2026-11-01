# Recall reaches subagents' chats

Each turn's input carries a recall block: the messages of the log that
best share the user's words, ranked by BM25 with no model call
(`Chat.shared`). Until now it read only the main chat. A question about
what a subagent did found its answer only through a search or a zoom.

`Agent.Recall` now adds up to three steps of the subagents' chats,
ranked together over all their logs as the main chat's messages are
ranked. Each line names the subagent and ends with the zoom that opens
the step whole:

    [Name]:83+1|echo, 2026-11-01: … zoom("Name:83", 1)

A step shows at most 600 bytes around its rarest word. The subagent's
task and report are left out, because the main chat holds both. The
main chat's lines are the same as before, and the view is untouched.
With `-agent-memory=false` the block is the main chat's alone.

## Measured

On a copy of a real chat (15 MB of log, 22 MB of subagent logs, 88
subagents), 18 questions with known answers, 8 lines from the main chat.
The probe is `TestRecallRealSize`. It makes no model call:

    OPT_RECALL_DIR=/path/to/copy OPT_RECALL_QUESTIONS=q.jsonl \
      go test -count=1 -run TestRecallRealSize -v .

| | main chat only | with subagents' steps |
|---|---|---|
| recall bytes, 18 questions | 157,365 | 191,115 (+21%, about 1.9 KB a turn) |
| questions whose answer is in the block | 4 | 9 |
| time per recall | 110–230 ms | 280–510 ms; 1.3 s the first time |

Recall runs while the call starts and is waited for only after Claude
Code's MCP servers settle, so the time is mostly hidden. The first
recall parses every log. Later ones reuse each log's parsed steps while
its size and time are unchanged (`agentMems.steps`). Parsing was half
the time before that cache.

A log that holds the questions themselves, such as an eval's own run,
makes the count look better than it is. Take such logs out of the copy
before measuring.
