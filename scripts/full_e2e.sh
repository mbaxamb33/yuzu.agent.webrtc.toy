#!/usr/bin/env bash
# Full end-to-end test: starts all services, creates a session, opens browser
set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
PORT="${PORT:-8080}"
BASE_URL="http://localhost:$PORT"
STARTUP_WAIT="${STARTUP_WAIT:-5}"

# Track PIDs for cleanup
PIDS=()

cleanup() {
    echo -e "\n${YELLOW}[cleanup] Stopping all services...${NC}"
    for pid in "${PIDS[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
        fi
    done
    # Also kill any child processes
    pkill -P $$ 2>/dev/null || true
    echo -e "${GREEN}[cleanup] Done${NC}"
}

trap cleanup EXIT INT TERM

log() {
    echo -e "${BLUE}[e2e]${NC} $1"
}

error() {
    echo -e "${RED}[e2e] ERROR: $1${NC}" >&2
}

success() {
    echo -e "${GREEN}[e2e] $1${NC}"
}

# Check for .env
if [ ! -f .env ]; then
    error ".env file not found"
    exit 1
fi

# Kill any orphaned processes from previous runs
log "Cleaning up any orphaned processes..."
for port in 8080 8081 8082 8083 8084 9090 9092 9093; do
    # lsof exits with 1 when nothing is listening; under set -e this would abort the script.
    # Swallow that non-zero status explicitly.
    pid=$(lsof -ti :$port 2>/dev/null || true)
    if [ -n "$pid" ]; then
        kill $pid 2>/dev/null || true
    fi
done
sleep 1

# Source environment
set -a
source .env
set +a

echo -e "${GREEN}"
echo "╔═══════════════════════════════════════════════════════════╗"
echo "║           FULL END-TO-END TEST                            ║"
echo "║                                                           ║"
echo "║  This will start all services and open a voice session    ║"
echo "╚═══════════════════════════════════════════════════════════╝"
echo -e "${NC}"

# Detect OS for STT socket path
if [[ "$(uname -s)" == "Darwin" ]]; then
    export STT_UDS_PATH="${STT_UDS_PATH:-/tmp/stt.sock}"
else
    export STT_UDS_PATH="${STT_UDS_PATH:-/run/app/stt.sock}"
fi

# Clean up old socket if exists
rm -f "$STT_UDS_PATH" 2>/dev/null || true

log "Starting services..."

# Start STT Sidecar
log "  [1/5] STT Sidecar (socket: $STT_UDS_PATH)"
go run ./cmd/stt-sidecar --uds "$STT_UDS_PATH" > /tmp/stt-sidecar.log 2>&1 &
PIDS+=($!)

# Start Orchestrator
log "  [2/5] Orchestrator (:9090)"
go run ./cmd/orchestrator > /tmp/orchestrator.log 2>&1 &
PIDS+=($!)

# Start LLM Service
log "  [3/5] LLM Service (:9092)"
go run ./cmd/llm > /tmp/llm.log 2>&1 &
PIDS+=($!)

# Start TTS Service
log "  [4/5] TTS Service (:9093)"
go run ./cmd/tts > /tmp/tts.log 2>&1 &
PIDS+=($!)

# Start API Server
log "  [5/5] API Server (:$PORT)"
go run ./cmd/server > /tmp/server.log 2>&1 &
PIDS+=($!)

log "Waiting ${STARTUP_WAIT}s for services to initialize..."
sleep "$STARTUP_WAIT"

# Health check
log "Checking service health..."
health_ok=true

check_health() {
    local name=$1
    local url=$2
    if curl -s -o /dev/null -w '' --connect-timeout 2 "$url" 2>/dev/null; then
        echo -e "  ${GREEN}✓${NC} $name"
    else
        echo -e "  ${RED}✗${NC} $name"
        health_ok=false
    fi
}

check_health "STT Sidecar" "http://localhost:8081/healthz"
check_health "Orchestrator" "http://localhost:8082/healthz"
check_health "LLM Service" "http://localhost:8083/healthz"
check_health "TTS Service" "http://localhost:8084/healthz"
check_health "API Server" "http://localhost:$PORT/health"

if [ "$health_ok" = false ]; then
    error "Some services failed to start. Check logs in /tmp/*.log"
    echo ""
    echo "Logs:"
    echo "  tail -f /tmp/stt-sidecar.log"
    echo "  tail -f /tmp/orchestrator.log"
    echo "  tail -f /tmp/llm.log"
    echo "  tail -f /tmp/tts.log"
    echo "  tail -f /tmp/server.log"
    exit 1
fi

success "All services healthy!"
echo ""

# Create session
log "Creating session..."
resp=$(curl -s -X POST "$BASE_URL/sessions")

session_id=$(echo "$resp" | sed -n 's/.*"session_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
room_url=$(echo "$resp" | sed -n 's/.*"room_url"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')

if [ -z "$session_id" ] || [ -z "$room_url" ]; then
    error "Failed to create session. Response: $resp"
    exit 1
fi

success "Session created!"
echo -e "  Session ID: ${YELLOW}$session_id${NC}"
echo -e "  Room URL:   ${YELLOW}$room_url${NC}"
echo ""

# Start bot
log "Starting bot worker..."
curl -s -X POST "$BASE_URL/sessions/$session_id/start" >/dev/null
success "Bot started!"
echo ""

# Open browser
log "Opening room in browser..."
if command -v open &>/dev/null; then
    open "$room_url"
elif command -v xdg-open &>/dev/null; then
    xdg-open "$room_url"
else
    echo -e "${YELLOW}Could not open browser. Open this URL manually:${NC}"
    echo "$room_url"
fi

echo ""
echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "${GREEN}                    SESSION ACTIVE                           ${NC}"
echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo ""
echo -e "  ${BLUE}What to do:${NC}"
echo "    1. Join the room in your browser (should open automatically)"
echo "    2. Allow microphone access"
echo "    3. The bot will greet you first"
echo "    4. Speak to the bot and wait for responses"
echo ""
echo -e "  ${BLUE}Useful commands (new terminal):${NC}"
echo "    Events:     curl -s $BASE_URL/sessions/$session_id/events | jq"
echo "    Stop:       curl -s -X POST $BASE_URL/sessions/$session_id/end"
echo "    Orch logs:  tail -f /tmp/orchestrator.log"
echo "    LLM logs:   tail -f /tmp/llm.log"
echo ""
echo -e "  ${YELLOW}Press Ctrl+C to stop all services${NC}"
echo ""
echo -e "${GREEN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"

# Keep running until interrupted
wait
