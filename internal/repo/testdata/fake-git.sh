#!/bin/sh
set -eu
if [ -n "${FAKE_GIT_ARGS:-}" ]; then printf '%s\n' "$@" > "$FAKE_GIT_ARGS"; fi
if [ -n "${FAKE_GIT_ENV:-}" ]; then env > "$FAKE_GIT_ENV"; fi
if [ "${FAKE_GIT_MODE:-ok}" = "sleep" ]; then
  sleep 300 & echo $! > "$FAKE_GIT_CHILD"; wait
fi
mkdir -p "$6"
printf 'readme\n' > "$6/README.md"
if [ -n "${FAKE_GIT_LINK:-}" ]; then ln -s "$FAKE_GIT_LINK" "$6/link"; fi
if [ -n "${FAKE_GIT_LINK_DIR:-}" ]; then ln -s "$FAKE_GIT_LINK_DIR" "$6/linkdir"; fi
