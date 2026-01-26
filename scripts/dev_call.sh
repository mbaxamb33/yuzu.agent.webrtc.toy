#!/usr/bin/env bash
# Interactive dev script: create session, start bot, open room in browser
set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:${PORT:-8080}}"

echo "=== Creating session ===" >&2
resp=$(curl -s -X POST "$BASE_URL/sessions")

session_id=$(echo "$resp" | sed -n 's/.*"session_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
room_url=$(echo "$resp" | sed -n 's/.*"room_url"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')

if [ -z "$session_id" ] || [ -z "$room_url" ]; then
  echo "ERROR: Failed to create session. Response:" >&2
  echo "$resp" >&2
  exit 1
fi

echo "Session ID: $session_id"
echo "Room URL:   $room_url"
echo ""

echo "=== Starting bot ===" >&2
curl -s -X POST "$BASE_URL/sessions/$session_id/start" >/dev/null
echo "Bot started (waiting for you to join)"
echo ""

# Open room URL in browser
echo "=== Opening room in browser ===" >&2
if command -v open &>/dev/null; then
  # macOS
  open "$room_url"
elif command -v xdg-open &>/dev/null; then
  # Linux
  xdg-open "$room_url"
else
  echo "Could not open browser automatically. Open this URL manually:"
  echo "$room_url"
fi

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Session: $session_id"
echo ""
echo "Useful commands:"
echo "  Events:  curl -s $BASE_URL/sessions/$session_id/events | jq"
echo "  Stop:    curl -s -X POST $BASE_URL/sessions/$session_id/end"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
