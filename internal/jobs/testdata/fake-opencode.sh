#!/bin/sh
set -eu
if [ -n "${FAKE_ARGS:-}" ]; then printf '%s\n' "$@" > "$FAKE_ARGS"; fi
if [ -n "${FAKE_ENV:-}" ]; then env > "$FAKE_ENV"; fi
if [ "${FAKE_MODE:-ok}" = "sleep" ]; then
  sleep 300 & echo $! > "$FAKE_CHILD"; wait
fi
target=$(printf '%s\n' "$@" | sed -n 's/^Target repo: //p' | head -1)
docs="${FAKE_DOCS_DIR:-$target/docs}"
mkdir -p "$docs"
printf 'codemap\n' > "$docs/codemap.md"
printf 'routing\n' > "$docs/routing.md"
printf 'observation\n' > "$docs/observation.md"
echo "Gist complete"
echo "Files: codemap.md routing.md observation.md"
exit "${FAKE_EXIT:-0}"
