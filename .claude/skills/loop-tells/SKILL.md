---
name: loop-tells
description: Model-check how tells reach a subagent in a loop (agents.go, loop.go) with TLA+ (formal/looptell). Use before committing any change to tell, queueHeldLocked, halt.queue, the replay or result handling in runAgent, loopStep or takeHeld, or rest's late tells, or when asked whether a tell to a looping subagent can be lost, doubled, reordered or left waiting, or whether its Stop hook can run while Claude Code still has a message.
---

# Tells to a subagent in a loop, model-checked

`formal/looptell/LoopTell.tla` is a TLA+ model of how opt-chat hands Opt's
tells to a subagent in a loop: `tell`, the call's goroutine reading Claude
Code's events (a replay as it takes a message, a turn's result), running
the Stop hook between turns, resting after its report and telling it
again with the tells its loop ended with; and Claude Code itself, as
measured on 2.1.318 with `--replay-user-messages`. TLC checks it over
every order of those steps within small bounds. The Go tests pin each
case TLC found; the model checks the orders no test writes.

## What it promises

| property | promise | harm if broken | Go test |
|---|---|---|---|
| Once | a tell reaches Claude Code once | the subagent does a thing twice | none: a mutant shows it can fail |
| InOrder | Claude Code takes tells and the hook's messages in the order they came; a tell between two turns goes ahead of the hook's next message | the subagent reads "do X" after "don't do X" | TestAgentLoopTellAndStop, TestAgentLoopLateOrder |
| TurnsAgree | the hook runs, its message goes and the subagent rests only between two turns: Claude Code idle, nothing on its input | the hook judges a turn that is not over; its message joins a turn it never saw; a second report | TestAgentLoopTellInTurn, TestAgentLoopTellsTogether |
| NoWaitInTurn | a tell waits for the next turn only between two turns, or before Claude Code's first echo | a tell sits for an hour-long turn | TestAgentLoopTellInTurn |
| NoHang | when only a new tell could move it, it is at rest and Claude Code took every tell and hook message | a tell lost; the loop waits for a turn that never comes | TestAgentLoopTellAndStop |

## Claude Code as modeled

Measured with stream-json in and out and `--replay-user-messages`
(`claude -p --model haiku`, a Bash `sleep` to make a step last):

- It takes its input in order.
- At the end of a step, in a turn, it takes the messages waiting, each
  echoed alone.
- Idle, it takes one or more as one turn, or each as a turn of its own.
  Of several taken as one, it echoed the first few alone, then all of
  them as one message: none alone, the first of two, the first two of
  three. The model takes any first few.
- An echo carries the `uuid` sent with its last message.

Check them again on a new Claude Code before trusting a pass: a replay
carrying the first message's uuid would make the model's queue wrong.

## Does a change touch it

| model | code |
|---|---|
| Tell, TellTs | `Agent.tell`: the wake from rest (`case !ok`, with `r.late` first), the held case, the in-turn send with `sendID` and `halt.queue` |
| Replay | `each` on `e.Replay`: `h.queued` cut at `e.UUID`; on the first, `queueHeldLocked` |
| Result | runAgent on a result: `more := len(h.queued) > 0`, `h.inTurn = more`, and with more, `queueHeldLocked` |
| Take1, Take2 | `loopStep`'s two `takeHeld` calls |
| Hook | `loopHook.stop`, a.mu free; `lp.turns` at its max |
| SendNext | `h.inTurn = true; p.send(next); queueHeldLocked` after `loopStep` |
| Stop, Late | the rest after the report: `rest.late`, then `a.tell(name)` |
| Begin, Step, End | Claude Code |

Left out: the user's message sent now (`tellNow`: `h.cut`, `h.now`, the
interrupt); compactions between turns (`lp.compacting`), at rest
(`r.compacting`, `r.pending`) and mid-task; a restart for granted MCP
servers (`h.restart`); a failed turn (`e.IsError`); who the turn is (the
user's or Opt's) and where the report goes; Stop on the page and the call
ending at rest; the call's first message's ack; empty tells.

Sync commit: the model describes the code at **06957d5**. To see if it
still does:

```sh
git diff 06957d5 -- loop.go
git diff 06957d5 -- agents.go claude.go | grep -nE 'held|inTurn|replays|queued|queue\(|late|sendID|sendAs|resting|UUID'
```

Empty output: the model still holds. Otherwise read the change against
the table. Move the sync commit forward only once you have confirmed the
model describes the code at the new commit.

## Run it

```sh
formal/looptell/check.sh     # MC.cfg and MCBig.cfg, seconds
formal/looptell/mutants.sh   # under a minute; every line must say "caught"
```

`formal/tlc.sh` downloads tla2tools once (checked by sha256); TLC needs
Java 11+. `check.sh` refuses a run under 10,000 states. A FAIL prints
the counterexample as the actions taken; `TRACE=1` prints whole states.
`mutants.sh` runs Python as `$PYTHON` (`python3` if unset).

The model describes the code as it is. The bugs it or review found are
mutants in `mutants.sh`: each puts one back and expects its property to
break. A new bug TLC finds becomes one more mutant once fixed, with a Go
test that failed before the fix.

When a change is intended, change the action to match, keep each action
one step between two lock operations, run `check.sh` and `mutants.sh`,
and move the sync commit.
