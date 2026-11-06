#!/usr/bin/env bash
# Model-checks AgentMem.tla against every property in PROPS, under each
# config given (default MC.cfg, MCCrash.cfg, then MCRestart.cfg). TLC
# stops at the first broken property; this prints its counterexample,
# drops it, and checks the rest again. Exit status 1 if any property breaks. TRACE=1 prints whole states.
set -euo pipefail
cd "$(dirname "$0")"
JAR=$(../tlc.sh)
PROPS=(NoWaitOnOthersIO NoDeadlock ChatIsLog NoLostStep FailureSurfaces Converges FailureStaysLocal)
CONFIGS=("$@")
[ ${#CONFIGS[@]} -eq 0 ] && CONFIGS=(MC.cfg MCCrash.cfg MCRestart.cfg)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0
for cfg in "${CONFIGS[@]}"; do
  left=("${PROPS[@]}")
  while [ ${#left[@]} -gt 0 ]; do
    { cat "$cfg"; printf 'INVARIANT %s\n' "${left[@]}"; } > "$tmp/run.cfg"
    start=$SECONDS
    if java -XX:+UseParallelGC -cp "$JAR" tlc2.TLC -workers auto -metadir "$tmp/meta" \
        -config "$tmp/run.cfg" AgentMem.tla > "$tmp/out" 2>&1; then
      n=$(grep -oE '[0-9]+ distinct states found' "$tmp/out" | tail -1 | grep -oE '^[0-9]+')
      if [ "$n" -lt 10000 ]; then # a step that can never be taken passes everything
        echo "$cfg ERROR: only $n states"; exit 2
      fi
      echo "$cfg PASS ${left[*]}: $(grep -oE '[0-9]+ distinct states found' "$tmp/out" | tail -1), depth $(grep -oE 'search is [0-9]+' "$tmp/out" | grep -oE '[0-9]+'), $((SECONDS - start))s"
      break
    fi
    fail=1
    p=$(grep -oE 'Invariant [A-Za-z]+ is violated' "$tmp/out" | head -1 | cut -d' ' -f2 || true)
    if [ -z "$p" ]; then
      echo "$cfg ERROR"; tail -20 "$tmp/out"; exit 2
    fi
    echo "$cfg FAIL $p:"
    grep -E '^State [0-9]+:' "$tmp/out" | sed -E 's/ line [0-9].*//; s/^State [0-9]+: <?/    /'
    if [ -n "${TRACE:-}" ]; then sed -n '/is violated/,/distinct states/p' "$tmp/out"; fi
    rm -rf "$tmp/meta"
    keep=()
    for q in "${left[@]}"; do [ "$q" = "$p" ] || keep+=("$q"); done
    left=("${keep[@]}")
  done
done
exit $fail
