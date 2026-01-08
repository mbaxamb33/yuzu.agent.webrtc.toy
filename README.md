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
