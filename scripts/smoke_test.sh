#!/usr/bin/env bash
# End-to-end smoke test of the built bgm binary.
#
# Runs the real binary in plain mode with a sandboxed data directory and the
# null audio backend, so nothing touches real user state and no sound is
# ever played. Verifies: playback ran, steering was accepted, a session was
# saved and resumed, and an MP3 export succeeded.
set -euo pipefail

cd "$(dirname "$0")/.."
BIN=./bgm
[ -x "$BIN" ] || { echo "build first: make build"; exit 1; }

command -v ffmpeg >/dev/null || { echo "ffmpeg required"; exit 1; }
command -v ffprobe >/dev/null || { echo "ffprobe required"; exit 1; }

SANDBOX="$(mktemp -d)"
trap 'rm -rf "$SANDBOX"' EXIT
export BGM_DATA_DIR="$SANDBOX/data"
export BGM_CONFIG_DIR="$SANDBOX/config"
export BGM_PLAYER_SPEED=25   # pace the null player faster than realtime

fail() { echo "SMOKE FAIL: $1"; exit 1; }

log="$BGM_DATA_DIR/logs/bgm.log"
out="$SANDBOX/out.txt"

# Drive a full interactive session through the plain interface. The noise
# engine keeps the run heavy-model-free; every other layer is the real one.
{
  echo "status"
  echo "generate pink noise"
  sleep 2
  echo "brown noise please"
  sleep 1
  echo "name smoke-session"
  echo "mp3 1"
  # Give the background export time to finish.
  sleep 6
  echo "sessions"
  echo "quit"
} | "$BIN" --engine noise --player null --plain > "$out" 2>&1 || fail "bgm exited non-zero"

grep -q "switching to pink noise" "$out" || fail "steering acknowledgment missing"
grep -q "switching to brown noise" "$out" || fail "second steering acknowledgment missing"
grep -q "session saved as smoke-session" "$out" || fail "session naming failed"
grep -q "smoke-session" "$BGM_DATA_DIR/sessions/smoke-session.json" || fail "session file missing"

# Playback actually ran: the log must show a noise source playing.
grep -q '"event":"now_playing"' "$log" || fail "no playback in log"
grep -q '"event":"steering"' "$log" || fail "no steering event in log"

# The export completed and is a valid MP3 of ~1 minute.
mp3=$(ls "$BGM_DATA_DIR"/exports/*.mp3 2>/dev/null | head -1)
[ -n "$mp3" ] || fail "no exported mp3 found"
codec=$(ffprobe -v error -select_streams a:0 -show_entries stream=codec_name -of csv=p=0 "$mp3")
[ "$codec" = "mp3" ] || fail "export codec is $codec"
dur=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$mp3")
awk -v d="$dur" 'BEGIN { exit !(d >= 58 && d <= 62) }' || fail "export duration $dur not ~60s"

# Resume the saved session by flag and confirm it comes back with the
# same steering context (brown noise).
resume_out="$SANDBOX/resume.txt"
{
  echo "status"
  sleep 1
  echo "quit"
} | "$BIN" --session smoke-session --player null --plain > "$resume_out" 2>&1 || fail "resume run exited non-zero"
grep -q "brown noise" "$resume_out" || fail "resumed session lost its steering context"

# Silence discipline: the null player was selected in both runs.
grep -q '"event":"player_selected","backend":"null"' "$log" || fail "null player not used"

echo "SMOKE OK"
