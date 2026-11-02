# Subagents of the user's own

The user can make a subagent from the page without asking Opt: Alt+N,
`/new` or the rail's `new` opens a menu above the bar for its folder,
model, effort, autocompact and name. It is meant to work like `claude`
started in that folder, while it still shows in the rail, full screen and
in Opt's chat as Opt's subagents do.

## What it gets

The same call as Opt's subagents: Opt's system prompt, tools and setup,
and the CLAUDE.md of its folder. What it lacks is the view and a task:
its first message is the user's, after a note that says whose it is,
that it has no view and no task, that its replies go to the user, and
where its tools run (`blankNote`). The view would cost up to about 40k
tokens on every first message, for chat context the user did not ask
for; it can still `zoom` or `search` the chat when asked.

Plain `claude` (Claude Code's own system prompt, the user's hooks, memory
and global CLAUDE.md) was the other choice. It would make the call a
different kind of call from every other opt-chat makes, with hooks that
Opt's calls keep out on purpose, so the user chose Opt's setup.

## How it runs

- `NewAgent` writes its record (`blank`, `dir`, state `new`) and its log's
  first step, which keeps the name, and lists it on the rail. No call
  starts.
- The first message, the user's from the page or Opt's `tell`, goes
  through `resume`, which for a record in state `new` starts its session
  with `--session-id` instead of resuming one. A first start that ends
  before Claude Code saved the session leaves the record `new`, so the
  next message starts it again. From there it is an
  ordinary subagent: at rest after each reply, resumed after its call
  ends, cut off and resumed by a restart.
- Its call runs in its folder (`startCall`'s `dir`); Claude Code's own
  servers kept out are those of that folder (`ccDenied`).
- Its window is the one chosen, with the `-agent-compact-max` cap left
  out: the user set it for this one subagent. 0 is off: Claude Code's
  auto-compaction is off in its call. Once compacted, its next message
  does not bring the view and a task again (`anchored`), since it had
  neither.
- Opt's chat gets one line saying who it is at its first message, and the
  usual `[user → Name]` line for each of the user's messages. Its replies
  to the user are not reported to Opt; a turn Opt's `tell` starts is.
- Opt's subagents leave the rail at a restart unless cut off or stopped,
  since Opt has their reports. The user's own stay for a day after their
  last use (`recoverAgents`), since the rail and its full screen are the
  only way the user reaches them. `clear` marks the record `cleared`, so
  a restart doesn't list it again; a message from Opt or the user puts it
  back.
