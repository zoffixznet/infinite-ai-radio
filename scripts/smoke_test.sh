#!/usr/bin/env bash
# End-to-end smoke test of the built iar binary.
#
# Runs the real binary in plain mode with a sandboxed data directory and the
# null audio backend, so nothing touches real user state and no sound is
# ever played. Verifies: playback ran, steering was accepted, a session was
# saved and resumed, and an MP3 export succeeded.
set -euo pipefail

cd "$(dirname "$0")/.."
BIN=./iar
[ -x "$BIN" ] || { echo "build first: make build"; exit 1; }

command -v ffmpeg >/dev/null || { echo "ffmpeg required"; exit 1; }
command -v ffprobe >/dev/null || { echo "ffprobe required"; exit 1; }

SANDBOX="$(mktemp -d)"
trap 'rm -rf "$SANDBOX"' EXIT
export IAR_DATA_DIR="$SANDBOX/data"
export IAR_CONFIG_DIR="$SANDBOX/config"
export IAR_PLAYER_SPEED=25   # pace the null player faster than realtime

fail() { echo "SMOKE FAIL: $1"; exit 1; }

log="$IAR_DATA_DIR/logs/iar.log"
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
} | "$BIN" --engine noise --player null --plain > "$out" 2>&1 || fail "iar exited non-zero"

grep -q "switching to pink noise" "$out" || fail "steering acknowledgment missing"
grep -q "switching to brown noise" "$out" || fail "second steering acknowledgment missing"
grep -q "session saved as smoke-session" "$out" || fail "session naming failed"
grep -q "smoke-session" "$IAR_DATA_DIR/sessions/smoke-session.json" || fail "session file missing"

# Playback actually ran: the log must show a noise source playing.
grep -q '"event":"now_playing"' "$log" || fail "no playback in log"
grep -q '"event":"steering"' "$log" || fail "no steering event in log"

# The export completed and is a valid MP3 of ~1 minute.
mp3=$(ls "$IAR_DATA_DIR"/exports/*.mp3 2>/dev/null | head -1)
[ -n "$mp3" ] || fail "no exported mp3 found"
codec=$(ffprobe -v error -select_streams a:0 -show_entries stream=codec_name -of csv=p=0 "$mp3")
[ "$codec" = "mp3" ] || fail "export codec is $codec"
dur=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$mp3")
awk -v d="$dur" 'BEGIN { exit !(d >= 58 && d <= 62) }' || fail "export duration $dur not ~60s"

# A stale auto-saved session from long ago must be swept at the next
# start; the named one must survive.
cat > "$IAR_DATA_DIR/sessions/session-20260101-000000.json" <<'JSON'
{"name":"session-20260101-000000","created":"2026-01-01T00:00:00Z","updated":"2026-01-01T00:00:00Z",
 "last_played":"2026-01-01T00:00:00Z","mode":"music","base_prompt":"old stale prompt","noise_bed":"pink"}
JSON

# Resume the saved session by flag and confirm it comes back with the
# same steering context (brown noise).
resume_out="$SANDBOX/resume.txt"
{
  echo "status"
  sleep 1
  echo "quit"
} | "$BIN" --session smoke-session --player null --plain > "$resume_out" 2>&1 || fail "resume run exited non-zero"
grep -q "brown noise" "$resume_out" || fail "resumed session lost its steering context"
[ ! -f "$IAR_DATA_DIR/sessions/session-20260101-000000.json" ] || fail "stale auto-saved session not swept"
grep -q '"event":"sessions_swept"' "$log" || fail "sweep not logged"
[ -f "$IAR_DATA_DIR/sessions/smoke-session.json" ] || fail "named session swept"

# Session management from the CLI: grouped listing, delete with and
# without --yes, preset tombstones and their restore.
list="$SANDBOX/list.txt"
"$BIN" sessions > "$list" 2>&1 || fail "sessions listing exited non-zero"
grep -q "^your sessions:" "$list" || fail "listing lacks the named group"
grep -q "^presets:" "$list" || fail "listing lacks the presets group"
grep -q "^auto-saved sessions:" "$list" || fail "listing lacks the auto group"
awk '/^your sessions:/{a=NR} /^presets:/{b=NR} /^auto-saved sessions:/{c=NR} END{exit !(a && b && c && a<b && b<c)}' "$list" \
  || fail "listing groups out of order"
grep -qE "^  smoke-session +brown noise +[0-9]+[smh] ago|^  smoke-session +brown noise +just now" "$list" \
  || fail "named row lacks summary/last played: $(grep smoke-session "$list")"
grep -qE "^  calm-piano +Gentle solo piano" "$list" || fail "preset row format"

echo n | "$BIN" sessions delete smoke-session | grep -q "cancelled" || fail "delete without confirmation did not cancel"
[ -f "$IAR_DATA_DIR/sessions/smoke-session.json" ] || fail "cancelled delete removed the session"
echo y | "$BIN" sessions delete smoke-session | grep -q "session smoke-session deleted" || fail "confirmed delete failed"
[ ! -f "$IAR_DATA_DIR/sessions/smoke-session.json" ] || fail "confirmed delete left the file"
"$BIN" sessions delete ghost-session --yes > "$SANDBOX/ghost.txt" 2>&1 && fail "deleting an unknown session succeeded"
grep -q "no session or preset named ghost-session" "$SANDBOX/ghost.txt" || fail "unknown delete message"

"$BIN" sessions delete sleep --yes | grep -q "preset sleep hidden" || fail "preset delete failed"
"$BIN" presets | grep -q "^  sleep " && fail "hidden preset still listed"
"$BIN" sessions | grep -q "^  sleep " && fail "hidden preset in the sessions listing"
"$BIN" --preset sleep --player null --plain < /dev/null > "$SANDBOX/hidden.txt" 2>&1 && fail "hidden preset could be started"
grep -q "restore-presets" "$SANDBOX/hidden.txt" || fail "hidden preset start lacks the restore hint"
"$BIN" sessions restore-presets | grep -q "restored 1 preset" || fail "restore-presets failed"
"$BIN" presets | grep -q "^  sleep " || fail "preset not restored"
"$BIN" sessions restore-presets | grep -q "nothing to restore" || fail "second restore not a no-op"

# Silence discipline: the null player was selected in both runs.
grep -q '"event":"player_selected","backend":"null"' "$log" || fail "null player not used"

echo "SMOKE OK"
