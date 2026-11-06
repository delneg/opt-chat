---------------------------- MODULE LoopTell ----------------------------
(* Tells to a subagent in a loop (agents.go, loop.go): Opt's tell, the
   subagent call's goroutine reading Claude Code's events and running the
   Stop hook between turns, and Claude Code itself. Every opt-chat action
   is one step of the code between two lock operations on Agent.mu; its
   comment names the code. mutants.sh puts back, one at a time, bugs this
   model or review found, and expects a property to catch each.

   Claude Code is modeled as measured on 2.1.318 with
   --replay-user-messages: it takes the messages on its input in order;
   at the end of a step, in a turn, any number of them, each echoed alone;
   when idle, one or more as one turn: the first few echoed alone (none,
   the first of two, the first two of three, as measured), then all of
   them again as one. Each echo carries the uuid of the last message it
   holds.

   A message is [id |-> n, items |-> s]: n its uuid (sendID), s what it
   holds in order: "f" the call's first message, a tell's name, or a hook
   message's name. *)
EXTENDS Naturals, Sequences, FiniteSets

CONSTANTS Tells,     \* Opt's tells, in the order Opt tells them
          MaxHook,   \* messages the Stop hook sends, at most 3
          MaxSteps   \* steps Claude Code ends in the whole call

HookItem == <<"h1", "h2", "h3">>
Res == [type |-> "result", msgs |-> <<>>, uuid |-> 0]

Range(s) == {s[i] : i \in DOMAIN s}
Min(S) == CHOOSE i \in S : \A j \in S : i <= j
Count(x, s) == Cardinality({i \in DOMAIN s : s[i] = x})
RECURSIVE Flat(_)
Flat(ms) == IF ms = <<>> THEN <<>> ELSE Head(ms).items \o Flat(Tail(ms))

VARIABLES
  cc,       \* Claude Code: "idle" or in a "turn"
  ccIn,     \* the messages on its input, not taken yet
  ev,       \* its events opt-chat has not read yet
  steps,    \* steps it has ended
  inTurn,   \* halt.inTurn
  replays,  \* halt.replays
  queued,   \* halt.queued: the messages sent in its turn, not echoed yet
  held,     \* a.held[name]: tells held between two turns
  resting,  \* a.resting[name] set: it reported, a tell sets it to work again
  pc,       \* the call's goroutine: runAgent's loop, loopStep
  next,     \* loopStep's result: the next message's items
  late,     \* rest.late: tells that came as its loop ended
  told, hooks, nextId,
  entered,  \* ghost: tells and hook messages in the order they came to opt-chat
  taken     \* ghost: what Claude Code took, in order

vars == <<cc, ccIn, ev, steps, inTurn, replays, queued, held, resting, pc, next,
          late, told, hooks, nextId, entered, taken>>

Msg(id, items) == [id |-> id, items |-> items]

Init ==
  /\ cc = "idle" /\ ccIn = <<Msg(1, <<"f">>)>> /\ ev = <<>> /\ steps = 0
  /\ inTurn = TRUE /\ replays = FALSE /\ queued = <<>>   \* p.send(second); h.inTurn = true
  /\ held = <<>> /\ resting = FALSE
  /\ pc = "read" /\ next = <<>> /\ late = <<>>
  /\ told = 0 /\ hooks = 0 /\ nextId = 2
  /\ entered = <<"f">> /\ taken = <<>>

-------------------------------------------------------------------------
(* Opt *)

\* tell(name, ts...) under a.mu: at rest, those its loop ended with go
\* first (r.late); ts not empty otherwise.
TellTs(ts) ==
  IF resting THEN                                 \* case !ok: at work again
       LET m == Msg(nextId, late \o ts) IN
       /\ resting' = FALSE /\ late' = <<>>
       /\ inTurn' = TRUE                          \* h.inTurn = true: one at rest is at work again
       /\ ccIn' = Append(ccIn, m) /\ nextId' = nextId + 1
       /\ queued' = Append(queued, m)             \* h.queue(id, msg)
       /\ UNCHANGED held
  ELSE IF ~inTurn \/ ~replays THEN                \* a.looping(name) && (!h.inTurn || !h.replays)
       /\ held' = held \o ts
       /\ UNCHANGED <<resting, late, inTurn, ccIn, nextId, queued>>
  ELSE LET m == Msg(nextId, ts) IN                \* in its turn: after its current step
       /\ ccIn' = Append(ccIn, m) /\ nextId' = nextId + 1
       /\ queued' = Append(queued, m)
       /\ UNCHANGED <<resting, late, inTurn, held>>

Tell ==
  /\ told < Len(Tells)
  /\ LET t == Tells[told + 1] IN
       /\ told' = told + 1
       /\ entered' = Append(entered, t)
       /\ TellTs(<<t>>)
  /\ UNCHANGED <<cc, ev, steps, replays, pc, next, hooks, taken>>

-------------------------------------------------------------------------
(* The call's goroutine: runAgent's loop over p.next(), and loopStep *)

\* each(e) on a replay, by its uuid: Claude Code takes its input in
\* order, so the message echoed and every one queued before it are taken.
DropUuid(q, u) ==
  LET I == {i \in DOMAIN q : q[i].id = u} IN
  IF I = {} THEN q ELSE SubSeq(q, Min(I) + 1, Len(q))
Matched(q, e) == DropUuid(q, e.uuid)

\* queueHeldLocked after the input in and the queue q: the held tells
\* sent as one message, noted queued.
FlushAfter(in, id, q) ==
  IF held = <<>> THEN ccIn' = in /\ nextId' = id /\ queued' = q /\ held' = held
  ELSE LET m == Msg(id, held) IN
       /\ ccIn' = Append(in, m) /\ nextId' = id + 1
       /\ queued' = Append(q, m) /\ held' = <<>>

\* each(e), e.Replay: taken, the tells in it are queued no more; on the
\* first, those held until now go in this turn.
Replay ==
  /\ pc = "read" /\ ev # <<>> /\ Head(ev).type = "replay"
  /\ ev' = Tail(ev)
  /\ replays' = TRUE
  /\ LET q == Matched(queued, Head(ev)) IN
       IF ~replays /\ inTurn THEN FlushAfter(ccIn, nextId, q)
       ELSE queued' = q /\ UNCHANGED <<held, ccIn, nextId>>
  /\ UNCHANGED <<cc, steps, inTurn, resting, pc, next, late, told, hooks, entered, taken>>

\* A result: more, a tell came too late for this turn, and Claude Code
\* runs it as the next: the hook runs once that one ends.
Result ==
  /\ pc = "read" /\ ev # <<>> /\ Head(ev).type = "result"
  /\ ev' = Tail(ev)
  /\ inTurn' = (queued # <<>>)
  /\ IF queued # <<>>
       THEN FlushAfter(ccIn, nextId, queued) /\ UNCHANGED pc
       ELSE pc' = "take1" /\ UNCHANGED <<ccIn, nextId, queued, held>>
  /\ UNCHANGED <<cc, steps, replays, resting, next, late, told, hooks, entered, taken>>

\* loopStep's first takeHeld: Opt's and the user's tells go first.
Take1 ==
  /\ pc = "take1"
  /\ IF held # <<>>
       THEN next' = held /\ held' = <<>> /\ pc' = "send"
       ELSE pc' = "hook" /\ UNCHANGED <<next, held>>
  /\ UNCHANGED <<cc, ccIn, ev, steps, inTurn, replays, queued, resting, late, told, hooks, nextId, entered, taken>>

\* The hook runs, a.mu free: it blocks, with a reason, or lets it stop.
\* At its cap (lp.turns >= max) it stops without running.
Hook ==
  /\ pc = "hook"
  /\ \/ hooks < MaxHook /\ pc' = "take2"
     \/ pc' = "stop"
  /\ UNCHANGED <<cc, ccIn, ev, steps, inTurn, replays, queued, held, resting, next, late, told, hooks, nextId, entered, taken>>

\* loopStep's second takeHeld, after the hook blocked: tells that came as
\* it ran go first; else the hook's reason is the next message.
Take2 ==
  /\ pc = "take2"
  /\ IF held # <<>>
       THEN next' = held /\ held' = <<>> /\ UNCHANGED <<hooks, entered>>
       ELSE /\ next' = <<HookItem[hooks + 1]>> /\ hooks' = hooks + 1
            /\ entered' = Append(entered, HookItem[hooks + 1]) /\ UNCHANGED held
  /\ pc' = "send"
  /\ UNCHANGED <<cc, ccIn, ev, steps, inTurn, replays, queued, resting, late, told, nextId, taken>>

\* h.inTurn = true; p.send(next), not queued; queueHeldLocked: those that
\* came between loopStep and here.
SendNext ==
  /\ pc = "send"
  /\ inTurn' = TRUE
  /\ FlushAfter(Append(ccIn, Msg(nextId, next)), nextId + 1, queued)
  /\ next' = <<>> /\ pc' = "read"
  /\ UNCHANGED <<cc, ev, steps, replays, resting, late, told, hooks, entered, taken>>

\* At rest: a.resting set, its late the tells held, under a.mu.
Stop ==
  /\ pc = "stop"
  /\ resting' = TRUE /\ late' = held /\ held' = <<>>
  /\ pc' = "late"
  /\ UNCHANGED <<cc, ccIn, ev, steps, inTurn, replays, queued, next, told, hooks, nextId, entered, taken>>

\* After its report, a.mu free: a.tell(name), unless a tell came first
\* and took them.
Late ==
  /\ pc = "late"
  /\ pc' = "read"
  /\ IF resting /\ late # <<>> THEN TellTs(<<>>)
     ELSE UNCHANGED <<resting, late, inTurn, ccIn, nextId, queued, held>>
  /\ UNCHANGED <<cc, ev, steps, replays, next, told, hooks, entered, taken>>

-------------------------------------------------------------------------
(* Claude Code *)

Echo(m) == [type |-> "replay", msgs |-> <<m>>, uuid |-> m.id]
EchoAll(ms) == [type |-> "replay", msgs |-> ms, uuid |-> ms[Len(ms)].id]

\* Idle, it takes k messages as one turn: the first j echoed alone, then
\* all of them.
Begin ==
  /\ cc = "idle" /\ ccIn # <<>>
  /\ \E k \in 1..Len(ccIn) : \E j \in 0..k - 1 :
       LET ms == SubSeq(ccIn, 1, k) IN
       /\ ccIn' = SubSeq(ccIn, k + 1, Len(ccIn))
       /\ ev' = ev \o [i \in 1..j |-> Echo(ms[i])] \o <<EchoAll(ms)>>
       /\ taken' = taken \o Flat(ms)
  /\ cc' = "turn"
  /\ UNCHANGED <<steps, inTurn, replays, queued, held, resting, pc, next, late, told, hooks, nextId, entered>>

\* A step ends: it takes k of its messages into the turn, each echoed alone.
Step ==
  /\ cc = "turn" /\ steps < MaxSteps
  /\ steps' = steps + 1
  /\ \E k \in 0..Len(ccIn) :
       LET ms == SubSeq(ccIn, 1, k) IN
       /\ ccIn' = SubSeq(ccIn, k + 1, Len(ccIn))
       /\ ev' = ev \o [i \in 1..k |-> Echo(ms[i])]
       /\ taken' = taken \o Flat(ms)
  /\ UNCHANGED <<cc, inTurn, replays, queued, held, resting, pc, next, late, told, hooks, nextId, entered>>

End ==
  /\ cc = "turn"
  /\ cc' = "idle" /\ ev' = Append(ev, Res)
  /\ UNCHANGED <<ccIn, steps, inTurn, replays, queued, held, resting, pc, next, late, told, hooks, nextId, entered, taken>>

Work == Replay \/ Result \/ Take1 \/ Hook \/ Take2 \/ SendNext \/ Stop \/ Late
        \/ Begin \/ Step \/ End
Next == Tell \/ Work
Spec == Init /\ [][Next]_vars

-------------------------------------------------------------------------
(* Properties *)

\* Every tell reaches Claude Code once: never sent twice, never two
\* copies on their way.
Once == \A x \in Range(entered) : Count(x, taken \o Flat(ccIn) \o held \o late \o next) <= 1

\* Claude Code takes tells and hook messages in the order they came:
\* a tell between two turns goes ahead of the hook's next message.
Pos(x) == CHOOSE i \in DOMAIN entered : entered[i] = x
InOrder == \A i, j \in DOMAIN taken : i < j => Pos(taken[i]) < Pos(taken[j])

\* opt-chat runs the hook, sends its message or rests only between two
\* turns: Claude Code idle, nothing on its input. Else the hook's message
\* would join a turn the hook did not see end, and a turn would end unseen.
TurnsAgree ==
  (pc \in {"take1", "hook", "take2", "send", "stop"} \/ resting)
    => cc = "idle" /\ ccIn = <<>> /\ ev = <<>>

\* A tell waits for the next turn only between two turns, or before
\* opt-chat knows Claude Code echoes what it takes.
NoWaitInTurn == held # <<>> /\ pc = "read" => ~replays

\* Nothing is stuck: when only a new tell could move it, the subagent is
\* at rest and Claude Code has taken everything that came.
Quiet ==
  /\ resting /\ pc = "read" /\ cc = "idle" /\ ccIn = <<>> /\ ev = <<>>
  /\ held = <<>> /\ Range(entered) \subseteq Range(taken)
NoHang == ~ENABLED Work => Quiet
=========================================================================
