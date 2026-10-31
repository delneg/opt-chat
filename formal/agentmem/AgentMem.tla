---------------------------- MODULE AgentMem ----------------------------
(* Subagents' memory (agentmem.go): each subagent's log fed into a chat of
   its own, zoomed, pumped, closed when idle, across a crash. Every action
   is one step of the code between two lock operations; its comment names
   the code. mutants.sh puts back, one at a time, bugs this model found
   while the code was written, and expects a property to catch each.

   A restart may turn -agent-memory either way (OffSwitch). Off, a step
   goes to the log alone; on again, the chat is fed from the log.

   A log is a sequence of writes, each what appendStep's one write left:
     [t |-> "full", id |-> n]  a whole step, ending in a blank line
     [t |-> "head", id |-> n]  cut short after its head (a crash, a full disk)
     [t |-> "pre"]             cut short inside its head
     [t |-> "seal"]            a blank line alone
   A chat is a sequence of messages [id |-> n, p |-> k]: logMsgs splits a
   step past clipMax into parts; here an even id has two. A crash or a full
   disk in logMsgs' one write (appendDay, then fatal) leaves its first
   lines: load skips a torn last line. *)
EXTENDS Naturals, Sequences, FiniteSets

CONSTANTS Subs,      \* subagents' names
          MaxSteps,  \* steps each subagent writes
          MaxCrash,  \* crashes of opt-chat, or failed writes
          MaxFail,   \* compactions and opens that fail
          OpenMax,   \* agentOpenMax
          OffSwitch  \* whether a restart may turn -agent-memory either way

NoLock == <<"", "">>                     \* no lock wanted
None == <<"none", "">>                   \* no holder; no zoom result yet
MM == <<"mm", "">>                       \* agentMems.mu
AM(s) == <<"am", s>>                     \* agentMem.mu
SC(s) == <<"sc", s>>                     \* the subagent chat's Chat.mu
Locks == {MM} \cup {AM(s) : s \in Subs} \cup {SC(s) : s \in Subs}

W(s) == <<"w", s>>                       \* the subagent's own goroutine: agentWriteRef
B(s) == <<"b", s>>                       \* a compaction of its chat ending: Chat.build
Z == <<"z", "">>                         \* Opt's zoom("Name"): ZoomAgent
P == <<"p", "">>                         \* jobGate.kick: agentMems.pump
Actors == {W(s) : s \in Subs} \cup {B(s) : s \in Subs} \cup {Z, P}

VARIABLES
  log,      \* agents/<Name>.log, on disk
  chat,     \* agents/<Name>.mem: the steps' ids, its messages' first parts, on disk
  read,     \* agentMem.read, in writes
  sealed,   \* agentMem.sealed (the fix): the log is known to end in a blank line
  ents,     \* agentMems.open: the names with an entry
  open,     \* those whose entry has its chat: am.c set
  busy,     \* a compaction of the chat underway (Chat.busy)
  pc, hold, \* each actor's place; each lock's holder
  tgt,      \* the subagent an actor works on: the zoom's, closeIdle's, pump's next
  plist,    \* the chats pump has left to visit
  steps, crashes, fails, nextId,
  noticed,  \* a memory failure said on the page (hub.notice)
  failed,   \* a memory failure happened
  zres,     \* what the last zoom told Opt
  on,       \* -agent-memory: FALSE, steps go to the log alone
  broken,   \* the subagent's chat stopped writing after a failed write (Chat.broken)
  killed    \* a subagent's memory took opt-chat down (fatal)

vars == <<log, chat, read, sealed, ents, open, busy, pc, hold, tgt, plist, steps,
          crashes, fails, nextId, noticed, failed, zres, on, broken, killed>>

-------------------------------------------------------------------------
(* parseSteps (subs.go): a head starts a step only at the text's start or
   after a blank line; the steps' ids, in order. *)
RECURSIVE Parse(_, _)
Parse(q, ab) ==
  IF q = <<>> THEN <<>>
  ELSE LET h == Head(q) IN
    CASE h.t = "full" -> (IF ab THEN <<h.id>> ELSE <<>>) \o Parse(Tail(q), TRUE)
      [] h.t = "head" -> (IF ab THEN <<h.id>> ELSE <<>>) \o Parse(Tail(q), FALSE)
      [] h.t = "pre"  -> Parse(Tail(q), FALSE)
      [] h.t = "seal" -> Parse(Tail(q), TRUE)

\* feed's cut at the last blank line: bytes.LastIndex(b, "\n\n")
RECURSIVE Ends(_)
Ends(q) == IF q = <<>> THEN 0
           ELSE IF q[Len(q)].t \in {"full", "seal"} THEN Len(q)
           ELSE Ends(SubSeq(q, 1, Len(q) - 1))

\* A step's messages, as logMsgs splits it.
Parts(id) == IF id % 2 = 0 THEN <<[id |-> id, p |-> 0], [id |-> id, p |-> 1]>>
             ELSE <<[id |-> id, p |-> 0]>>
RECURSIVE Expand(_)
Expand(ids) == IF ids = <<>> THEN <<>> ELSE Parts(Head(ids)) \o Expand(Tail(ids))
\* The parts a crash cut off the chat's last step.
Missing(c) == IF c = <<>> THEN <<>>
              ELSE LET m == c[Len(c)] IN SubSeq(Parts(m.id), m.p + 2, Len(Parts(m.id)))
Held(c) == Cardinality({i \in 1..Len(c) : c[i].p = 0})

Unread(s) == SubSeq(log[s], read[s] + 1, Len(log[s]))
Chunk(s) == SubSeq(Unread(s), 1, Ends(Unread(s)))
\* What feed logs: the chunk's steps, less those the chat holds when opened.
New(s) == LET st == Parse(Chunk(s), TRUE)
          IN IF read[s] = 0 THEN SubSeq(st, Held(chat[s]) + 1, Len(st)) ELSE st
\* A chat opened again first ends the step whose parts a crash cut off.
ToFeed(s) == (IF read[s] = 0 THEN Missing(chat[s]) ELSE <<>>) \o Expand(New(s))
Feed(s) == /\ chat' = [chat EXCEPT ![s] = @ \o ToFeed(s)]
           /\ read' = [read EXCEPT ![s] = @ + Len(Chunk(s))]

Subj(a) == IF a[1] \in {"w", "b"} THEN a[2] ELSE tgt[a]

WantOf(a) ==
  CASE pc[a] \in {"lock", "openm", "ci", "pm"} -> MM
    [] pc[a] = "am"    -> AM(Subj(a))
    [] pc[a] = "sc"    -> SC(Subj(a))
    [] pc[a] = "view"  -> SC(tgt[a])
    [] pc[a] = "bsc"   -> SC(a[2])
    [] pc[a] = "psc"   -> SC(tgt[a])
    [] pc[a] = "ciam"  -> AM(tgt[a])
    [] OTHER -> NoLock

Free(l) == hold[l] = None
Take(a, ls) == hold' = [l \in Locks |-> IF l \in ls THEN a ELSE hold[l]]
Give(ls) == hold' = [l \in Locks |-> IF l \in ls THEN None ELSE hold[l]]
Go(a, p) == pc' = [pc EXCEPT ![a] = p]

\* What a restart forgets: all but the files.
Restart == /\ read' = [s \in Subs |-> 0] /\ sealed' = [s \in Subs |-> FALSE]
           /\ ents' = {} /\ open' = {} /\ busy' = [s \in Subs |-> FALSE]
           /\ pc' = [a \in Actors |-> "idle"] /\ hold' = [l \in Locks |-> None]
           /\ plist' = {} /\ broken' = [s \in Subs |-> FALSE]
           /\ UNCHANGED tgt
-------------------------------------------------------------------------
(* agentMems.lock: m.mu held to find the entry, then the entry's mu. *)
Lock(a) == /\ pc[a] = "lock" /\ Free(MM) /\ Go(a, "am")
           /\ ents' = ents \cup {Subj(a)}
           /\ UNCHANGED <<log, chat, read, sealed, open, busy, hold, tgt, plist, steps, crashes, fails, nextId, noticed, failed, zres, broken, killed, on>>
LockAm(a) == /\ pc[a] = "am" /\ Free(AM(Subj(a))) /\ Take(a, {AM(Subj(a))})
             /\ Go(a, IF a[1] = "w" THEN "append" ELSE "feed")
             /\ UNCHANGED <<log, chat, read, sealed, ents, open, busy, tgt, plist, steps, crashes, fails, nextId, noticed, failed, zres, broken, killed, on>>

\* Its own I/O on its log, holding its am.mu. A log cut short gets a blank
\* line before the step: on, unless this entry wrote its end; off, always.
Written(s) == LET full == [t |-> "full", id |-> nextId]
                  ends == log[s] = <<>> \/ log[s][Len(log[s])].t \in {"full", "seal"}
              IN IF ~ends /\ (~on \/ ~sealed[s]) THEN <<[t |-> "seal"], full>> ELSE <<full>>

\* Off: agentWriteRef → appendStep on a nil agentMems: appendRaw alone.
StartWrite(s) == /\ pc[W(s)] = "idle" /\ steps[s] < MaxSteps /\ Go(W(s), IF on THEN "lock" ELSE "append")
                 /\ steps' = [steps EXCEPT ![s] = @ + 1]
                 /\ UNCHANGED <<log, chat, read, sealed, ents, open, busy, hold, tgt, plist, crashes, fails, nextId, noticed, failed, zres, broken, killed, on>>
WriteStep(s) == /\ pc[W(s)] = "append"
             /\ log' = [log EXCEPT ![s] = @ \o Written(s)]
             /\ sealed' = [sealed EXCEPT ![s] = TRUE]
             /\ nextId' = nextId + 1
             /\ Go(W(s), IF on THEN "feed" ELSE "idle")
             /\ UNCHANGED <<chat, read, ents, open, busy, hold, tgt, plist, steps, crashes, fails, noticed, failed, zres, broken, killed, on>>
\* appendRaw failing part way: a full disk. The step is lost, said in the terminal.
TornWrite(s) == /\ pc[W(s)] = "append" /\ crashes < MaxCrash
                /\ \E t \in {[t |-> "head", id |-> nextId], [t |-> "pre"]} :
                     log' = [log EXCEPT ![s] = Append(@, t)]
                /\ sealed' = [sealed EXCEPT ![s] = FALSE]
                /\ crashes' = crashes + 1 /\ nextId' = nextId + 1
                /\ Give({AM(s)}) /\ Go(W(s), IF on THEN "ci" ELSE "idle")
                /\ UNCHANGED <<chat, read, ents, open, busy, tgt, plist, steps, fails, noticed, failed, zres, broken, killed, on>>

(* agentMems.fed, am.mu held. An open chat: feed under its mu (logMsgs).
   None: openChat, feed it before anyone can see it, then m.mu and its mu
   to set am.c. openChat or a read can fail. *)
FeedStep(a) ==
  LET s == Subj(a) IN
  /\ pc[a] = "feed"
  /\ \/ /\ s \in open /\ ~broken[s] /\ Go(a, "sc")
        /\ UNCHANGED <<chat, read, failed, fails, noticed, zres, hold, broken, open, on>>
     \* fed: a chat that stopped writing is closed, and opened again here.
     \/ /\ s \in open /\ broken[s]
        /\ open' = open \ {s} /\ read' = [read EXCEPT ![s] = 0] /\ broken' = [broken EXCEPT ![s] = FALSE]
        /\ UNCHANGED <<chat, failed, fails, noticed, zres, hold, pc, broken, killed, on>>
     \/ /\ s \notin open /\ Feed(s) /\ Go(a, "openm")
        /\ UNCHANGED <<failed, fails, noticed, zres, hold, open, broken, on>>
     \/ /\ s \notin open /\ fails < MaxFail
        /\ fails' = fails + 1 /\ failed' = TRUE
        \* said on the page; ZoomAgent says why
        /\ noticed' = TRUE
        /\ zres' = IF a = Z THEN <<"error", "">> ELSE zres
        /\ Give({AM(s)}) /\ Go(a, IF a = Z THEN "idle" ELSE "ci")
        /\ UNCHANGED <<chat, read, open, broken, on>>
  /\ UNCHANGED <<log, sealed, ents, busy, tgt, plist, steps, crashes, nextId, killed, on>>
LockSc(a) == /\ pc[a] = "sc" /\ Free(SC(Subj(a))) /\ Take(a, {SC(Subj(a))}) /\ Go(a, "logio")
             /\ UNCHANGED <<log, chat, read, sealed, ents, open, busy, tgt, plist, steps, crashes, fails, nextId, noticed, failed, zres, broken, killed, on>>
\* logMsgs: appendDay and saveView fsync with the chat's mu held.
\* A chat that stopped writing since fed looked (a compaction's write
\* failed) keeps the messages in memory only; fed, after the feed, closes it.
LogIO(a) == LET s == Subj(a) IN
            /\ pc[a] = "logio" /\ Give({SC(s), AM(s)}) /\ Go(a, IF a = Z THEN "view" ELSE "ci")
            /\ IF broken[s]
                 THEN UNCHANGED chat /\ broken' = [broken EXCEPT ![s] = FALSE] /\ open' = open \ {s} /\ read' = [read EXCEPT ![s] = 0]
                 ELSE Feed(s) /\ UNCHANGED <<open, broken, on>>
            /\ UNCHANGED <<log, sealed, ents, busy, tgt, plist, steps, crashes, fails, nextId, noticed, failed, zres, killed, on>>
\* logMsgs' write failing (a full disk): its first k lines are on disk.
\* fed closes the chat, says so, and the step's write goes on; zoom gives
\* the whole log. opt-chat goes on (the main chat's failed writes stop it).
MemFail(a) == LET s == Subj(a) IN
  /\ pc[a] = "logio" /\ ~broken[s] /\ fails < MaxFail /\ fails' = fails + 1 /\ failed' = TRUE
  /\ \E k \in 0..(Len(ToFeed(s)) - 1) : chat' = [chat EXCEPT ![s] = @ \o SubSeq(ToFeed(s), 1, k)]
  /\ open' = open \ {s} /\ read' = [read EXCEPT ![s] = 0] /\ noticed' = TRUE
  /\ Give({SC(s), AM(s)}) /\ Go(a, IF a = Z THEN "idle" ELSE "ci")
  /\ zres' = IF a = Z THEN <<"plain", "">> ELSE zres
  /\ UNCHANGED <<log, sealed, ents, busy, tgt, plist, steps, crashes, nextId, on, broken, killed>>
OpenSet(a) == LET s == Subj(a) IN
              /\ pc[a] = "openm" /\ Free(MM)
              /\ open' = open \cup {s}
              /\ Give({AM(s)}) /\ Go(a, IF a = Z THEN "view" ELSE "ci")
              /\ UNCHANGED <<log, chat, read, sealed, ents, busy, tgt, plist, steps, crashes, fails, nextId, noticed, failed, zres, broken, killed, on>>

(* closeIdle, after am.mu is given back: m.mu over the whole pass. *)
CloseIdle(a) ==
  /\ pc[a] = "ci" /\ Free(MM)
  /\ IF Cardinality(ents) <= OpenMax \/ ents \subseteq {Subj(a)}
       THEN Go(a, "idle") /\ UNCHANGED <<hold, tgt, broken, killed, on>>
       ELSE \E t \in ents \ {Subj(a)} :
              IF Free(AM(t))       \* am.mu.TryLock
                THEN Take(a, {MM, AM(t)}) /\ tgt' = [tgt EXCEPT ![a] = t] /\ Go(a, "cisc")
                ELSE Go(a, "idle") /\ UNCHANGED <<hold, tgt, broken, killed, on>>
  /\ UNCHANGED <<log, chat, read, sealed, ents, open, busy, plist, steps, crashes, fails, nextId, noticed, failed, zres, broken, killed, on>>
\* am.mu.Lock() in place of TryLock: what the lock order forbids. No code
\* takes this step; mutants.sh puts it in to show NoDeadlock can fail.
CloseAm(a) == /\ pc[a] = "ciam" /\ Free(AM(tgt[a])) /\ Take(a, {AM(tgt[a])}) /\ Go(a, "cisc")
              /\ UNCHANGED <<log, chat, read, sealed, ents, open, busy, tgt, plist, steps, crashes, fails, nextId, noticed, failed, zres, broken, killed, on>>
\* sc.mu.TryLock(), as for am.mu.
CloseSc(a) ==
  LET t == tgt[a] IN
  /\ pc[a] = "cisc"
  /\ IF Free(SC(t)) /\ ~busy[t]
       THEN ents' = ents \ {t} /\ open' = open \ {t} /\ read' = [read EXCEPT ![t] = 0] /\ sealed' = [sealed EXCEPT ![t] = FALSE]
       ELSE UNCHANGED <<ents, open, read, sealed, broken, killed, on>>
  /\ Give({MM, AM(t)}) /\ Go(a, "idle")
  /\ UNCHANGED <<log, chat, busy, tgt, plist, steps, crashes, fails, nextId, noticed, failed, zres, broken, killed, on>>

(* ZoomAgent. *)
StartZoom == /\ pc[Z] = "idle" /\ zres = None /\ on
             /\ \E s \in Subs : log[s] # <<>> /\ tgt' = [tgt EXCEPT ![Z] = s]
             /\ Go(Z, "lock")
             /\ UNCHANGED <<log, chat, read, sealed, ents, open, busy, hold, plist, steps, crashes, fails, nextId, noticed, failed, zres, broken, killed, on>>
View == /\ pc[Z] = "view" /\ Free(SC(tgt[Z])) /\ Go(Z, "idle")
        \* One closed since it was fed is read from its messages in memory: whole.
        /\ zres' = <<"view", IF tgt[Z] \in open /\ Missing(chat[tgt[Z]]) # <<>> THEN "torn" ELSE "">>
        /\ UNCHANGED <<log, chat, read, sealed, ents, open, busy, hold, tgt, plist, steps, crashes, fails, nextId, noticed, failed, broken, killed, on>>

(* agentMems.pump: the chats copied under m.mu, their mus taken without it. *)
StartPump == /\ pc[P] = "idle" /\ open # {} /\ (\E s \in open : ~busy[s] /\ chat[s] # <<>>)
             /\ Free(MM) /\ plist' = open
             /\ UNCHANGED hold
             /\ Go(P, "pnext")
             /\ UNCHANGED <<log, chat, read, sealed, ents, open, busy, tgt, steps, crashes, fails, nextId, noticed, failed, zres, broken, killed, on>>
PumpNext == /\ pc[P] = "pnext"
            /\ IF plist = {}
                 THEN Go(P, "idle") /\ UNCHANGED hold /\ UNCHANGED <<tgt, plist, broken, killed, on>>
                 ELSE \E t \in plist : tgt' = [tgt EXCEPT ![P] = t] /\ plist' = plist \ {t} /\ Go(P, "psc") /\ UNCHANGED hold
            /\ UNCHANGED <<log, chat, read, sealed, ents, open, busy, steps, crashes, fails, nextId, noticed, failed, zres, broken, killed, on>>
\* Chat.pump under its mu: a compaction starts (jobGate.take).
PumpSc == LET t == tgt[P] IN
          /\ pc[P] = "psc" /\ Free(SC(t)) /\ Go(P, "pnext")
          /\ busy' = [busy EXCEPT ![t] = @ \/ (t \in open /\ chat[t] # <<>>)]
          /\ UNCHANGED <<log, chat, read, sealed, ents, open, hold, tgt, plist, steps, crashes, fails, nextId, noticed, failed, zres, broken, killed, on>>

(* Chat.build: the model's call without locks, then its mu over writing
   the line (flushNodes, saveView: fsync). A failure warns (subChat.warn). *)
BuildEnd(s) == /\ pc[B(s)] = "idle" /\ busy[s] /\ Go(B(s), "bsc")
               /\ UNCHANGED <<log, chat, read, sealed, ents, open, busy, hold, tgt, plist, steps, crashes, fails, nextId, noticed, failed, zres, broken, killed, on>>
BuildSc(s) == /\ pc[B(s)] = "bsc" /\ Free(SC(s)) /\ Take(B(s), {SC(s)}) /\ Go(B(s), "bio")
              /\ UNCHANGED <<log, chat, read, sealed, ents, open, busy, tgt, plist, steps, crashes, fails, nextId, noticed, failed, zres, broken, killed, on>>
BuildIO(s) == /\ pc[B(s)] = "bio" /\ Give({SC(s)}) /\ Go(B(s), "idle")
              /\ busy' = [busy EXCEPT ![s] = FALSE]
              /\ \/ UNCHANGED <<fails, failed, noticed, broken, killed, on>>
                 \/ fails < MaxFail /\ fails' = fails + 1 /\ failed' = TRUE /\ noticed' = TRUE
              /\ UNCHANGED <<log, chat, read, sealed, ents, open, tgt, plist, steps, crashes, nextId, zres, broken, killed, on>>
\* flushNodes' or saveView's write failing: the chat stops writing.
BuildFail(s) == /\ pc[B(s)] = "bio" /\ fails < MaxFail /\ fails' = fails + 1 /\ failed' = TRUE
                /\ broken' = [broken EXCEPT ![s] = TRUE] /\ noticed' = TRUE
                /\ busy' = [busy EXCEPT ![s] = FALSE] /\ Give({SC(s)}) /\ Go(B(s), "idle")
                /\ UNCHANGED <<log, chat, read, sealed, ents, open, tgt, plist, steps, crashes, nextId, zres, killed, on>>

(* opt-chat dies (or /reload execs, or a full disk makes appendDay fatal):
   what is on disk stays. A crash in a write to a log leaves it cut short;
   one in logMsgs leaves the chat its first k new messages. The restart may
   turn -agent-memory either way. *)
Crash == /\ crashes < MaxCrash /\ crashes' = crashes + 1
         /\ \E s \in Subs, cut \in {<<>>, <<[t |-> "head", id |-> nextId]>>, <<[t |-> "pre"]>>} :
              /\ cut # <<>> => pc[W(s)] = "append"
              /\ log' = [log EXCEPT ![s] = @ \o cut]
         /\ \/ UNCHANGED chat
            \/ \E a \in Actors, k \in 1..2 :
                 /\ pc[a] = "logio" /\ k < Len(ToFeed(Subj(a)))
                 /\ chat' = [chat EXCEPT ![Subj(a)] = @ \o SubSeq(ToFeed(Subj(a)), 1, k)]
         /\ on' \in IF OffSwitch THEN BOOLEAN ELSE {on}
         /\ nextId' = nextId + 1 /\ Restart
         /\ UNCHANGED <<steps, failed, fails, noticed, zres, killed>>

\* The name's log gone (by hand), its .mem left: a new subagent spawned
\* under the name. No code takes this step: spawn skips a name with a .log
\* or a .mem. mutants.sh puts it in to show ChatIsLog can fail.
Reuse(s) == /\ crashes < MaxCrash /\ crashes' = crashes + 1
            /\ log[s] # <<>> /\ \A a \in Actors : pc[a] = "idle"
            /\ log' = [log EXCEPT ![s] = <<>>] /\ steps' = [steps EXCEPT ![s] = 0]
            /\ Restart
            /\ UNCHANGED <<chat, failed, fails, nextId, noticed, zres, on, killed>>

ZoomAgain == /\ pc[Z] = "idle" /\ zres # None /\ zres' = None
             /\ UNCHANGED <<log, chat, read, sealed, ents, open, busy, pc, hold, tgt, plist, steps, crashes, fails, nextId, noticed, failed, broken, killed, on>>

Init == /\ log = [s \in Subs |-> <<>>] /\ chat = [s \in Subs |-> <<>>]
        /\ read = [s \in Subs |-> 0] /\ sealed = [s \in Subs |-> FALSE]
        /\ ents = {} /\ open = {} /\ busy = [s \in Subs |-> FALSE]
        /\ pc = [a \in Actors |-> "idle"] /\ hold = [l \in Locks |-> None]
        /\ tgt = [a \in Actors |-> CHOOSE s \in Subs : TRUE] /\ plist = {}
        /\ steps = [s \in Subs |-> 0] /\ crashes = 0 /\ fails = 0 /\ nextId = 1
        /\ noticed = FALSE /\ failed = FALSE /\ zres = None
        /\ on \in IF OffSwitch THEN BOOLEAN ELSE {TRUE}
        /\ broken = [s \in Subs |-> FALSE] /\ killed = FALSE

Actor(a) == Lock(a) \/ LockAm(a) \/ FeedStep(a) \/ LockSc(a) \/ LogIO(a) \/ MemFail(a) \/ OpenSet(a)
            \/ CloseIdle(a) \/ CloseAm(a) \/ CloseSc(a)
Run == \/ \E s \in Subs : StartWrite(s) \/ WriteStep(s) \/ TornWrite(s) \/ Actor(W(s))
                          \/ BuildEnd(s) \/ BuildSc(s) \/ BuildIO(s) \/ BuildFail(s)
       \/ StartZoom \/ Actor(Z) \/ View \/ ZoomAgain
       \/ StartPump \/ PumpNext \/ PumpSc
Next == Run \/ Crash

Spec == Init /\ [][Next]_vars

\* States equal but for whom an idle actor last worked on are one state.
StateView == <<log, chat, read, sealed, ents, open, busy, pc, hold, plist, steps, crashes, fails,
               nextId, noticed, failed, zres, on, broken, killed, [a \in Actors |-> IF pc[a] = "idle" THEN "" ELSE tgt[a]]>>

-------------------------------------------------------------------------
Blocked(a) == WantOf(a) # NoLock /\ hold[WantOf(a)] \notin {None, a}
RECURSIVE Waits(_, _)
\* Whom a waits on, lock by lock.
Waits(a, n) == IF n = 0 \/ ~Blocked(a) THEN {}
               ELSE LET h == hold[WantOf(a)] IN {h} \cup Waits(h, n - 1)
InIO(a) == pc[a] \in {"append", "logio", "bio"} \/ (pc[a] = "feed" /\ Subj(a) \notin open)

\* A subagent's step and Opt's zoom never wait on another subagent's disk.
NoWaitOnOthersIO == \A a \in {W(s) : s \in Subs} \cup {Z} :
                      \A h \in Waits(a, Cardinality(Actors)) : InIO(h) => Subj(h) = Subj(a)
\* No two wait on each other: the lock order holds.
NoDeadlock == \A a \in Actors : a \notin Waits(a, Cardinality(Actors))
All(s) == Expand(Parse(SubSeq(log[s], 1, Ends(log[s])), TRUE))
\* The chat holds the log's steps, in order, none twice, message by
\* message: it is always a first part of what the whole log reads as.
ChatIsLog == \A s \in Subs : Len(chat[s]) <= Len(All(s)) /\ chat[s] = SubSeq(All(s), 1, Len(chat[s]))
\* Whatever crashed where, and whichever way the restarts turned the off
\* switch: once fed to the log's end, an open chat is the whole log, as if
\* nothing had happened; and zoom never shows a step cut short.
Converges == /\ \A s \in Subs : s \in open /\ read[s] = Ends(log[s]) => chat[s] = All(s)
             /\ zres # <<"view", "torn">>
\* Every step written whole reads back as a step of its own.
NoLostStep == \A s \in Subs : \A i \in 1..Len(log[s]) :
                log[s][i].t = "full" =>
                  \E j \in 1..Len(Parse(log[s], TRUE)) : Parse(log[s], TRUE)[j] = log[s][i].id
\* A failure of subagents' memory is said on the page, and zoom never says
\* a subagent that has a log has none.
FailureSurfaces == /\ failed => noticed \/ killed
                   /\ zres # <<"no such subagent", "">>
\* A failure in a subagent's memory never stops opt-chat.
FailureStaysLocal == ~killed
=========================================================================
