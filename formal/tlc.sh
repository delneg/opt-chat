#!/usr/bin/env bash
# Prints the path of the pinned TLA+ tools jar, fetched and checked once
# into the cache. Every model's check.sh runs TLC from it. TLA_JAR names
# another jar.
set -euo pipefail
VER=1.7.4
SUM=936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88
JAR=${TLA_JAR:-${XDG_CACHE_HOME:-$HOME/.cache}/tla/tla2tools-$VER.jar}
if [ ! -f "$JAR" ]; then
  mkdir -p "$(dirname "$JAR")"
  curl -fsSL -o "$JAR.tmp" "https://github.com/tlaplus/tlaplus/releases/download/v$VER/tla2tools.jar"
  echo "$SUM  $JAR.tmp" | sha256sum -c --quiet >&2
  mv "$JAR.tmp" "$JAR"
fi
echo "$JAR"
