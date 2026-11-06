#!/usr/bin/env bash
# Shows each property of LoopTell.tla can fail: puts back, one at a time,
# a bug this model or review found, and expects check.sh to report that
# property broken. Each mutant is one or more find-and-replace pairs on
# the spec, read from stdin: a find, a line "=>", its replacement, and
# "--" between pairs. PYTHON names the Python to run (python3).
# Exit status 1 if a mutant goes uncaught.
set -euo pipefail
cd "$(dirname "$0")"
miss=0
mutant() { # name, property, config; the pairs on stdin
  local name=$1 prop=$2 cfg=$3 d
  d=$(mktemp -d)
  mkdir "$d/looptell"
  cp ../tlc.sh "$d/"
  cp LoopTell.tla MC.tla ./*.cfg check.sh "$d/looptell/"
  ${PYTHON:-python3} -c '
import sys
p, pairs = sys.argv[1], sys.stdin.read()
s = open(p).read()
for pair in pairs.split("\n--\n"):
    a, b = pair.split("\n=>\n")
    assert s.count(a) == 1, "mutant text not found once: " + a
    s = s.replace(a, b.rstrip("\n"))
open(p, "w").write(s)
' "$d/looptell/LoopTell.tla"
  sed -i.orig "s/^PROPS=.*/PROPS=($prop)/" "$d/looptell/check.sh" # a suffix: GNU's sed and macOS's alike
  if "$d/looptell/check.sh" "$cfg" > "$d/out" 2>&1; then
    echo "MISSED $prop ($name)"; miss=1
  elif grep -q "FAIL $prop" "$d/out"; then
    echo "caught $prop ($name)"
  else
    echo "ERROR $prop ($name)"; cat "$d/out"; miss=1
  fi
  rm -rf "$d"
}

# A replay matched queued tells by their first text: the first queued
# that its text starts with. Claude Code echoes the first of two it takes
# as one turn twice, so with two tells alike a third still waiting lost
# its place, and the hook ran with it on Claude Code's input.
mutant text TurnsAgree MC.cfg <<'EOF'
Matched(q, e) == DropUuid(q, e.uuid)
=>
Text(x) == IF x \in {"t1", "t3"} THEN "same" ELSE IF x \in {"h1", "h2", "h3"} THEN "hook" ELSE x
Remove(q, i) == SubSeq(q, 1, i - 1) \o SubSeq(q, i + 1, Len(q))
RECURSIVE DropText(_, _)
DropText(q, ms) ==
  IF ms = <<>> THEN q
  ELSE LET I == {i \in DOMAIN q : Text(q[i].items[1]) = Text(Head(ms).items[1])} IN
       DropText(IF I = {} THEN q ELSE Remove(q, Min(I)), Tail(ms))
Matched(q, e) == DropText(q, e.msgs)
EOF

# The tells its loop ended with went after its report as a tell of their
# own: one that came first, at rest, went ahead of them.
mutant late InOrder MC.cfg <<'EOF'
       LET m == Msg(nextId, late \o ts) IN
       /\ resting' = FALSE /\ late' = <<>>
=>
       LET m == Msg(nextId, IF ts = <<>> THEN late ELSE ts) IN
       /\ resting' = FALSE /\ late' = IF ts = <<>> THEN <<>> ELSE late
--
  /\ IF resting /\ late # <<>> THEN TellTs(<<>>)
=>
  /\ IF late # <<>> /\ ~resting THEN
       LET m == Msg(nextId, late) IN
       /\ ccIn' = Append(ccIn, m) /\ nextId' = nextId + 1
       /\ queued' = Append(queued, m) /\ late' = <<>>
       /\ UNCHANGED <<resting, inTurn, held>>
     ELSE IF late # <<>> THEN TellTs(<<>>)
EOF

# A tell lost the tells its loop ended with: they stayed at rest.
mutant lost NoHang MC.cfg <<'EOF'
  /\ IF resting /\ late # <<>> THEN TellTs(<<>>)
=>
  /\ IF FALSE THEN TellTs(<<>>)
EOF

# One a tell set to work from rest was not marked in its turn: the next
# tells waited for that turn's end (6b09ea1).
mutant wake NoWaitInTurn MC.cfg <<'EOF'
       /\ inTurn' = TRUE                          \* h.inTurn = true: one at rest is at work again
=>
       /\ UNCHANGED inTurn
EOF

# A result always ran the hook, even with a tell Claude Code had not
# taken yet: it ran that one as a turn the hook never saw (2319651).
mutant more TurnsAgree MC.cfg <<'EOF'
  /\ inTurn' = (queued # <<>>)
  /\ IF queued # <<>>
=>
  /\ inTurn' = FALSE
  /\ IF FALSE
EOF

# A tell sent in a turn was not noted as queued.
mutant queue TurnsAgree MC.cfg <<'EOF'
       /\ queued' = Append(queued, m)
       /\ UNCHANGED <<resting, late, inTurn, held>>
=>
       /\ UNCHANGED <<queued, resting, late, inTurn, held>>
EOF

# The tells held before the first replay waited for the turn's end.
mutant first NoWaitInTurn MC.cfg <<'EOF'
       IF ~replays /\ inTurn THEN FlushAfter(ccIn, nextId, q)
=>
       IF FALSE THEN FlushAfter(ccIn, nextId, q)
EOF

# Tells that came as the hook ran went after its message.
mutant take2 InOrder MC.cfg <<'EOF'
  /\ pc = "take2"
  /\ IF held # <<>>
=>
  /\ pc = "take2"
  /\ IF FALSE
EOF

# Tells that came as the hook's message was being sent waited for the
# next turn.
mutant sendheld NoWaitInTurn MC.cfg <<'EOF'
  /\ FlushAfter(Append(ccIn, Msg(nextId, next)), nextId + 1, queued)
=>
  /\ ccIn' = Append(ccIn, Msg(nextId, next)) /\ nextId' = nextId + 1
  /\ UNCHANGED <<queued, held>>
EOF

# The held tells, once sent, stayed held.
mutant twice Once MC.cfg <<'EOF'
       /\ queued' = Append(q, m) /\ held' = <<>>
=>
       /\ queued' = Append(q, m) /\ held' = held
EOF
exit $miss
