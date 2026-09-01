#!/usr/bin/env bash
# Re-shoot the README's phone-remote screenshots.
#
# Drives the real remote in headless Firefox against a sandboxed player
# with invented demo content, then wraps each frame in a phone body.
# Nothing it starts outlives it: every process it spawns is tagged, and
# the tag is hunted down on the way out however the run ends - success,
# failure, timeout or Ctrl-C.
#
#   scripts/screenshots.sh [output-dir]     (default: assets/)
#
# Needs: go, geckodriver, firefox, pactl, ffmpeg, python3 with Pillow.

set -uo pipefail

cd "$(dirname "$0")/.."
OUT=${1:-assets}
RAW=$(mktemp -d "${TMPDIR:-/tmp}/iar-shots-XXXXXX")

# Every process this run starts inherits this marker, which is what
# makes the cleanup below precise: it can never match your own radio.
export IAR_SHOOT_ID="iar-shoot-$$-$(date +%s)"

mem() { awk '/MemAvailable/{printf "%d", $2/1024}' /proc/meminfo 2>/dev/null || echo "?"; }

cleanup() {
  local rc=$? killed=0 pid
  for dir in /proc/[0-9]*; do
    pid=${dir#/proc/}
    [ "$pid" = "$$" ] && continue
    if grep -qz "IAR_SHOOT_ID=$IAR_SHOOT_ID" "/proc/$pid/environ" 2>/dev/null; then
      kill -9 "$pid" 2>/dev/null && killed=$((killed + 1))
    fi
  done
  # The shoot loads a null audio sink per run so Firefox stays silent.
  if command -v pactl >/dev/null 2>&1; then
    for m in $(pactl list short modules 2>/dev/null | grep 'iar-browser-test' | cut -f1); do
      pactl unload-module "$m" 2>/dev/null && killed=$((killed + 1))
    done
  fi
  rm -rf "$RAW"
  [ "$killed" -gt 0 ] && echo "cleanup: reclaimed $killed stray process(es)/sink(s)"
  echo "memory available: ${MEM_BEFORE}MB before, $(mem)MB after"
  exit $rc
}
trap cleanup EXIT INT TERM

missing=""
for tool in go geckodriver firefox pactl ffmpeg python3; do
  command -v "$tool" >/dev/null 2>&1 || missing="$missing $tool"
done
if [ -n "$missing" ]; then
  echo "screenshots need:$missing" >&2
  echo "geckodriver: https://github.com/mozilla/geckodriver/releases" >&2
  exit 1
fi
if ! python3 -c 'import PIL' 2>/dev/null; then
  echo "screenshots need Pillow: pip install --user Pillow" >&2
  exit 1
fi

MEM_BEFORE=$(mem)
echo "shooting into $RAW (run tag $IAR_SHOOT_ID)"

# The timeout is a backstop: if the browser wedges, the trap still runs.
if ! IAR_SHOTS="$RAW" timeout --signal=TERM --kill-after=30s 10m \
    go test -tags browser -count=1 -run TestScreenshots -timeout 9m ./internal/remote/; then
  echo "the shoot failed; $OUT left untouched" >&2
  exit 1
fi

python3 scripts/phone_frame.py "$RAW" "$OUT" || exit 1
echo "wrote $(ls -1 "$OUT"/*.png 2>/dev/null | wc -l) framed screenshots to $OUT/"
