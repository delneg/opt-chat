#!/usr/bin/env bash
# Shows each property of AgentMem.tla can fail: puts back, one at a time,
# a bug the model found while the code was written (or a change the lock
# order forbids), and expects check.sh to report that property broken.
# Each mutant is one or more find-and-replace pairs on the spec, read from
# stdin: a find, a line "=>", its replacement, and "--" between pairs.
# Exit status 1 if a mutant goes uncaught.
set -euo pipefail
cd "$(dirname "$0")"
miss=0
mutant() { # name, property, config; the pairs on stdin
  local name=$1 prop=$2 cfg=$3 d
  d=$(mktemp -d)
  mkdir "$d/agentmem"
  cp ../tlc.sh "$d/"
  cp AgentMem.tla ./*.cfg check.sh "$d/agentmem/"
  python3 -c '
import sys
p, pairs = sys.argv[1], sys.stdin.read()
s = open(p).read()
for pair in pairs.split("\n--\n"):
    a, b = pair.split("\n=>\n")
    assert s.count(a) == 1, "mutant text not found once: " + a
    s = s.replace(a, b.rstrip("\n"))
open(p, "w").write(s)
' "$d/agentmem/AgentMem.tla"
  sed -i "s/^PROPS=.*/PROPS=($prop)/" "$d/agentmem/check.sh"
  if "$d/agentmem/check.sh" "$cfg" > "$d/out" 2>&1; then
    echo "MISSED $prop ($name)"; miss=1
  elif grep -q "FAIL $prop" "$d/out"; then
    echo "caught $prop ($name)"
  else
    echo "ERROR $prop ($name)"; cat "$d/out"; miss=1
  fi
  rm -rf "$d"
}

# The pump held m.mu over every chat's mu.
mutant pump NoWaitOnOthersIO MC.cfg <<'EOF'
             /\ UNCHANGED hold
             /\ Go(P, "pnext")
=>
             /\ Take(P, {MM})
             /\ Go(P, "pnext")
--
Go(P, "idle") /\ UNCHANGED hold /\ UNCHANGED
=>
Go(P, "idle") /\ Give({MM}) /\ UNCHANGED
EOF

# closeIdle waited on a chat's mu, holding m.mu.
mutant close NoWaitOnOthersIO MC.cfg <<'EOF'
    [] pc[a] = "ciam"  -> AM(tgt[a])
=>
    [] pc[a] = "cisc"  -> SC(tgt[a])
    [] pc[a] = "ciam"  -> AM(tgt[a])
--
  /\ pc[a] = "cisc"
  /\ IF Free(SC(t))
=>
  /\ pc[a] = "cisc" /\ Free(SC(t))
  /\ IF Free(SC(t))
EOF

# A log cut short took the next step's head for its text.
mutant seal NoLostStep MCCrash.cfg <<'EOF'
IN IF ~ends /\ (~on \/ ~sealed[s]) THEN
=>
IN IF ~ends /\ ~on THEN
EOF

# With -agent-memory=false, a log cut short was not sealed.
mutant offseal NoLostStep MCRestart.cfg <<'EOF'
IN IF ~ends /\ (~on \/ ~sealed[s]) THEN
=>
IN IF ~ends /\ on /\ ~sealed[s] THEN
EOF

# A memory that failed to open was told to the terminal alone, and zoom
# said there was no such subagent.
mutant notice FailureSurfaces MCCrash.cfg <<'EOF'
        /\ noticed' = TRUE
        /\ zres' = IF a = Z THEN <<"error", "">> ELSE zres
=>
        /\ noticed' = FALSE
        /\ zres' = IF a = Z THEN <<"no such subagent", "">> ELSE zres
EOF

# A chat opened again counted a step cut after its first part as held.
mutant parts Converges MCCrash.cfg <<'EOF'
ToFeed(s) == (IF read[s] = 0 THEN Missing(chat[s]) ELSE <<>>) \o Expand(New(s))
=>
ToFeed(s) == Expand(New(s))
EOF

# A failed write to a subagent's chat was fatal.
mutant nofatal FailureStaysLocal MCRestart.cfg <<'EOF'
                /\ broken' = [broken EXCEPT ![s] = TRUE] /\ noticed' = TRUE
                /\ busy' = [busy EXCEPT ![s] = FALSE] /\ Give({SC(s)}) /\ Go(B(s), "idle")
                /\ UNCHANGED <<log, chat, read, sealed, ents, open, tgt, plist, steps, crashes, nextId, zres, killed, on>>
=>
                /\ killed' = TRUE /\ Restart /\ UNCHANGED <<log, chat, noticed, zres, on, crashes, nextId, steps>>
EOF

# Spawn gave a name whose .mem outlived its .log to a new subagent.
mutant reuse ChatIsLog MCRestart.cfg <<'EOF'
Next == Run \/ Crash
=>
Next == Run \/ Crash \/ \E s \in Subs : Reuse(s)
EOF

# fed did not check, after a feed, for a compaction's write failing in it.
mutant recheck Converges MCRestart.cfg <<'EOF'
THEN UNCHANGED chat /\ broken' = [broken EXCEPT ![s] = FALSE] /\ open' = open \ {s} /\ read' = [read EXCEPT ![s] = 0]
=>
THEN UNCHANGED <<chat, broken, open>> /\ read' = [read EXCEPT ![s] = @ + Len(Chunk(s))]
EOF

# A feed read the chat's steps without leaving out those it holds.
mutant skip ChatIsLog MC.cfg <<'EOF'
IN IF read[s] = 0 THEN SubSeq(st, Held(chat[s]) + 1, Len(st)) ELSE st
=>
IN st
EOF

# closeIdle waited on a subagent's own mu, against the lock order.
mutant order NoDeadlock MC.cfg <<'EOF'
THEN Take(a, {MM, AM(t)}) /\ tgt' = [tgt EXCEPT ![a] = t] /\ Go(a, "cisc")
=>
THEN Take(a, {MM}) /\ tgt' = [tgt EXCEPT ![a] = t] /\ Go(a, "ciam")
EOF
exit $miss
