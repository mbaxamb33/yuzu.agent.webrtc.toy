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

echo "Real-VAD mode: join the room and speak over the bot." >&2

deadline=$(( $(date +%s) + 180 ))
seen_vad=0
seen_stop=0
seen_tts_interrupted=0
seen_latency=0

while [ $(date +%s) -lt $deadline ]; do
  events=$(curl -s "$BASE_URL/sessions/$session_id/events")
  grep -q '"vad_start"' <<< "$events" && seen_vad=1 || true
  grep -q '"stop_tts_sent"' <<< "$events" && seen_stop=1 || true
  grep -q '"tts_stopped"' <<< "$events" && grep -q '"interrupted"' <<< "$events" && seen_tts_interrupted=1 || true
  grep -q '"barge_in_latency"' <<< "$events" && seen_latency=1 || true

  if [ $seen_vad -eq 1 ] && [ $seen_stop -eq 1 ] && [ $seen_tts_interrupted -eq 1 ] && [ $seen_latency -eq 1 ]; then
    echo "Barge-in flow observed (real VAD)" >&2
    exit 0
  fi
  sleep 2
done

echo "Timeout waiting for real VAD barge-in sequence" >&2
exit 1

