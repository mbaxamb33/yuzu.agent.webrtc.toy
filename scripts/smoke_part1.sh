#!/usr/bin/env bash
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:${PORT:-8080}}"

echo "Creating session..." >&2
resp=$(curl -s -X POST "$BASE_URL/sessions")
session_id=$(echo "$resp" | sed -n 's/.*"session_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
room_url=$(echo "$resp" | sed -n 's/.*"room_url"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
echo "session_id: $session_id"
echo "room_url:   $room_url"

echo "Starting bot..." >&2
curl -s -X POST "$BASE_URL/sessions/$session_id/start" >/dev/null

echo "Polling events for TTS_DONE and PUBLISHED_AUDIO_FRAMES..." >&2
deadline=$(( $(date +%s) + 120 ))
while [ $(date +%s) -lt $deadline ]; do
  events=$(curl -s "$BASE_URL/sessions/$session_id/events")
  if echo "$events" | grep -q '"TTS_DONE"' && echo "$events" | grep -q 'PUBLISHED_AUDIO_FRAMES='; then
    echo "TTS_DONE and PUBLISHED_AUDIO_FRAMES observed" >&2
    exit 0
  fi
  sleep 2
done
echo "Timeout waiting for TTS_DONE + PUBLISHED_AUDIO_FRAMES" >&2
exit 1
