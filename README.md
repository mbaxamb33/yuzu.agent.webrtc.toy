# AI Voice Interviewer (Daily + Deepgram + ElevenLabs) — Loop A First

An AI voice interviewer that runs inside a **Daily** room and feels truly real-time.

**Core principle:**  
- **Loop A (real-time control) is the product.**  
- **Loop B (LLM intelligence/scoring) comes after Loop A is solid.**

---

## What “real-time” means here

The system must support:

- **AI speaks in-room** via **ElevenLabs TTS**
- **AI listens continuously** via **Daily audio** → **Deepgram Streaming ASR**
- **Barge-in**: the candidate can interrupt and the AI **stops speaking immediately**
- Produces:
  - **Structured transcript**
  - **Event timeline** (who spoke, when, interruptions, turn boundaries, etc.)
  - Later: **report**

## Dev quickstart

- Build: `make build` (outputs `bin/server`)
- Run: `make server` (uses `go run ./cmd/server`)
- Test: `make test`
- Format: `make fmt` | Lint: `make vet`

Env vars you can set when running (optional):

- `SERVER_ADDR` (default `:8080`)
- `DAILY_DOMAIN_URL` (e.g. `https://your-team.daily.co`)
- `DAILY_API_KEY`, `DEEPGRAM_API_KEY`, `ELEVENLABS_API_KEY`

Example:

```bash
SERVER_ADDR=":8081" DAILY_DOMAIN_URL="https://your-team.daily.co" make server
```

## Proto code generation

Shared proto lives at `proto/stt.proto`.

- Go stubs:
  - Requirements: `protoc`, `protoc-gen-go`, `protoc-gen-go-grpc`
  - Generate all: `make proto-go` (stt, gateway_control, llm, tts)

- Python stubs:
  - Requirements: `grpcio-tools` (dev‑only)
  - Install: `pip install -r dev-requirements.txt`
  - Generate all: `make proto-py` (outputs to `gateway/` and patches imports)

Runtime Python dependencies are under `gateway/requirements.txt`; `grpcio-tools` is used only for code generation.

## Gateway (Python) and STT Sidecar

- Gateway (Python) handles Daily I/O and control only. Policy (VAD, barge‑in, LLM, TTS decisions) lives in Go services.
- STT uses the Go sidecar; builtin Python STT has been removed.

- Run sidecar locally (UDS at `/run/app/stt.sock` by default):
  - `make sidecar`
- Build sidecar binary:
  - `make sidecar-build` (outputs `bin/stt-sidecar`)

Gateway configuration to use sidecar:

```bash
STT_ENABLED=true STT_UDS_PATH=/run/app/stt.sock python -m gateway.main
```

Note: `STT_MODE` is deprecated; sidecar is the only supported mode.

## CI

- Proto drift check runs in CI (`.github/workflows/proto-check.yml`):
  - Regenerates Go and Python stubs and fails if there are diffs.

## Dev convenience

- Start all Go services locally:
  - `make all-services` (runs STT sidecar, Orchestrator, LLM, and TTS; Ctrl+C to stop)
