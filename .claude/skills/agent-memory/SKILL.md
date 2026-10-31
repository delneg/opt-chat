---
name: agent-memory
description: Model-check opt-chat's subagent memory (agentmem.go) with TLA+ (formal/agentmem). Use before committing any change to agentmem.go, to the agentWriteRef/appendStep path in agents.go, or to Chat.pump, Chat.build or Chat.logMsgs in chat.go, or when asked whether a subagent's step can be lost, doubled or swallowed, whether one subagent can hold another up, or whether its locks can deadlock.
---

# Subagent memory, model-checked

`formal/agentmem/AgentMem.tla` is a TLA+ model of how opt-chat feeds each
subagent's log into a chat of its own (`agentmem.go`): the subagent's own
goroutine writing a step and feeding it, Opt's `zoom("Name")`, the pump
that gives chats their compactions, a compaction saving its line,
closing idle chats past `agentOpenMax`, a write cut short, a write to a
chat that fails, and a crash or restart that may turn `-agent-memory`
either way.
TLC checks it over every order of those steps within small bounds. The Go
tests pin each case TLC found; the model checks the orders no test writes.

## What it promises

| property | promise | harm if broken | Go test |
|---|---|---|---|
| NoWaitOnOthersIO | a subagent's step or Opt's zoom never waits, lock by lock, on another subagent's disk I/O | one slow fsync or one long first zoom holds up every subagent | TestAgentMemPumpNoStall, TestAgentMemCloseNoStall, TestAgentMemNoGlobalStall |
| NoDeadlock | no actors wait on each other in a cycle: the lock order on `agentMems` holds | opt-chat hangs | none: a mutant shows it can fail |
| ChatIsLog | a subagent's chat is always a first part of what its whole log parses as, message by message: no step lost, doubled or out of order, across reopens and crashes | Opt reads a step twice, or never | TestAgentMemCloses, TestAgentMemTornWrite, TestAgentMemNameReuse |
| Converges | after a crash or a failed write (a full disk) at any step and restarts with memory on or off, an open chat fed to the log's end is the whole log, as with no crash; zoom never shows a step cut short | part of a long step is lost for good after a crash | TestAgentMemTornParts, TestAgentMemWriteFails |
| FailureStaysLocal | a failed write to a subagent's chat never stops opt-chat | a full disk kills the main chat over a subagent's memory | TestAgentMemWriteFails |
| NoLostStep | every step written whole parses as a step of its own, memory on or off | a step after a cut-short write vanishes into the cut one's text | TestAgentMemTornTail, TestAgentMemOffSeals |
| FailureSurfaces | a failure of a subagent's memory shows on the page; zoom never says a subagent with a log doesn't exist | a broken memory goes unseen, and Opt thinks the work never happened | TestAgentMemOpenFails, TestAgentMemFailureNoticed |

The off switch (`-agent-memory=false`: no chats, the old whole-log zoom)
is modeled as far as the log goes: off, a step is appended alone, and a
restart may turn memory on and feed the chat from the log. TestAgentMemOff
checks the zoom it gives.

## Does a change touch it

The model's steps and the code they stand for. Each action is one step
of the code between two lock operations.

| model | code |
|---|---|
| Lock, LockAm | `agentMems.lock` (m.mu to find the entry, then the entry's mu) |
| StartWrite, WriteStep, TornWrite | `agentWriteRef` (agents.go) → `agentMems.appendStep`: the seal (`sealed`, `endsBlank`, `agentMem.sealed`), `appendRaw` |
| FeedStep, LockSc, LogIO, OpenSet | `agentMems.fed` and `feed`; `Chat.logMsgs` holds the chat's mu over `appendDay` and `saveView` (fsync); `closeBroken` before and after the feed |
| MemFail | `appendDay` or `saveView` failing in a subagent's chat (a full disk): `Chat.failWrite`; `closeBroken` |
| BuildFail | `flushNodes` or `saveView` failing at a compaction's end: `Chat.broken` set |
| Reuse | a name whose `.log` is gone and `.mem` stays, given to a new subagent: no code takes it (`Agent.freeName` skips such names); a mutant puts it in |
| CloseIdle, CloseSc | `agentMems.closeIdle`: TryLock of the entry's mu, then of the chat's |
| StartZoom, View | `Agent.ZoomAgent`, `memFailed` |
| StartPump, PumpNext, PumpSc | `agentMems.pump`, kicked by `jobGate.give` and `jobGate.setMain`; `Chat.pump` |
| BuildEnd, BuildSc, BuildIO | `Chat.build` after the model call: the chat's mu over `flushNodes` and `refit` (fsync); `Chat.warn` |
| Crash | the process dies, `/reload` execs, or the main chat's write is fatal: entries, `read` and `sealed` go; the log and `.mem` stay, each maybe cut short; the restart may turn `-agent-memory` either way |
| `Parts`, `Missing`, `Held` | `logMsgs`' split at `clipMax` (an even id has two parts); `feed`'s count of held steps and the parts it writes back |
| `Parse`, `Ends` | `parseSteps`/`stepHeads` (subs.go); feed's cut at the last `"\n\n"` |

Left out: `jobGate`'s count (its mu is never held across another lock),
`setCompact` (start, quit, reload: it still holds m.mu over every chat's
mu), the main chat, a step's size and its thinking, a torn line inside
the `.mem` files (load skips it, and the step is fed again), the tree's
lines (a lost one is built again), a crash inside the first feed of a
chat being opened (only `logio` writes are cut), the chat's lock file (a
failed open closes it: TestAgentMemWriteFails, disk full), and the check of
`Chat.broken` before a feed: no rule needs it, it only spares a zoom
the whole log after a compaction's write failed.

Sync commit: the model describes the code at **c258251**. To see if it
still does:

```sh
git diff c258251 -- agentmem.go
git diff c258251 -- agents.go chat.go subs.go | grep -nE 'appendStep|appendRaw|freeName|failWrite|broken|func \(c \*Chat\) (pump|build|logMsgs|appendDay|saveView|flushNodes|idle)|stepHeads|parseSteps|openChatAs'
```

Empty output: the model still holds. Otherwise read the change against
the table. Move the sync commit forward only once you have confirmed the
model describes the code at the new commit.

## Run it

```sh
formal/agentmem/check.sh      # MC.cfg ~6 min, MCCrash.cfg and MCRestart.cfg seconds, on 2 cores
formal/agentmem/mutants.sh    # under a minute; every line must say "caught"
```

`formal/tlc.sh` downloads tla2tools 1.7.4 once (checked by sha256) into
`~/.cache/tla`; TLC needs Java 11+. `check.sh` refuses a run under 10,000
states: a step that can never be taken passes every property. A FAIL
prints the counterexample as the actions taken; `TRACE=1` prints whole
states.

The model describes the code as it is. The bugs it found while the code
was written are mutants in `mutants.sh`: each puts one back and expects
its property to break. A new bug TLC finds becomes one more mutant once
fixed, with a Go test that failed before the fix.

## Read a result

The `/statechart` skill draws a counterexample as a path in plain words,
if you have it. A counterexample is a real bug when you can replay it
through `agentWrite`, `ZoomAgent`/`zoom` and `agentMems.pump` in a Go
test; every one found so far did, but for a race in `fed`'s second check,
which only the model pins (the `recheck` mutant). It is drift in the
model when the code checks a condition the action doesn't: fix the
model, never the property.

When a change is intended (a new lock, a new step), change the action to
match, keep each action one step between two lock operations, run
`check.sh` and `mutants.sh`, and move the sync commit.
