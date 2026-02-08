# Yuzu Voice Agent - Setup Guide

Complete setup instructions for running the voice agent locally.

## Prerequisites

**macOS/Linux required.** Windows not supported (uses Unix domain sockets).

---

## 1. Install Go (1.21+)

```bash
# macOS
brew install go

# Ubuntu/Debian
sudo apt update && sudo apt install -y golang-go

# Verify
go version
```

---

## 2. Install Python (3.10+)

```bash
# macOS
brew install python@3.11

# Ubuntu/Debian
sudo apt install -y python3.11 python3.11-venv python3-pip

# Verify
python3 --version
```

---

## 3. Install protobuf compiler (for gRPC)

```bash
# macOS
brew install protobuf

# Ubuntu/Debian
sudo apt install -y protobuf-compiler

# Verify
protoc --version
```

---

## 4. Clone the repo

```bash
git clone https://github.com/mbaxamb33/yuzu.agent.webrtc.toy.git
cd yuzu.agent.webrtc.toy
```

---

## 5. Install Go dependencies

```bash
go mod download
```

---

## 6. Create Python virtual environment

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install --upgrade pip
pip install -r gateway/requirements.txt
pip install grpcio-tools  # for proto generation
```

---

## 7. Create `.env` file

Create a `.env` file in the project root with your API keys:

```bash
# Server
PORT=8080

# Daily.co (get from https://dashboard.daily.co)
DAILY_API_KEY=your_daily_api_key_here
DAILY_DOMAIN=yourdomain.daily.co
DAILY_ROOM_PREFIX=ai-interview-v2-
DAILY_ROOM_PRIVACY=public
DAILY_ENABLE_MUSIC_MODE=true
DAILY_AUDIO_BITRATE=96000
DAILY_ENABLE_DTX=false

# ElevenLabs (get from https://elevenlabs.io)
ELEVENLABS_API_KEY=your_elevenlabs_api_key_here
ELEVENLABS_VOICE_ID=CwhRBWXzGAHq8TQ4Fs17
ELEVENLABS_STREAMING=true
ELEVENLABS_CANNED_PHRASE="Hello and welcome! I'm your AI interviewer today."

# Deepgram (get from https://console.deepgram.com)
DEEPGRAM_API_KEY=your_deepgram_api_key_here
STT_ENABLED=true

# Azure OpenAI (get from Azure Portal)
AZURE_OPENAI_API_KEY=your_azure_openai_key_here
AZURE_OPENAI_ENDPOINT=https://your-resource.openai.azure.com/
AZURE_OPENAI_API_VERSION=2024-02-15-preview
AZURE_OPENAI_DEPLOYMENT=gpt-4o-mini

# Bot worker
BOT_WORKER_CMD="./.venv/bin/python3 -m gateway.main"
BOT_IDLE_EXIT_SECONDS=60
WORKER_TOKEN_SECRET=yuzu-worker-secret-change-me

# STT settings
STT_MIN_RMS=75
STT_SILENCE_RMS_FLOOR=20
STT_BATCH_MS=60
STT_CONTINUOUS=true
STT_KEEPALIVE_MS=400

# Barge-in settings
LOCAL_STOP_MIN_RMS=1400
LOCAL_STOP_GUARD_MS=1200
WORKER_VAD_MIN_START_FRAMES_WHILE_TTS=10
WORKER_VAD_AGGRESSIVENESS=3
WORKER_VAD_HANGOVER_MS=200
LOCAL_STOP_REQUIRE_INTERIM=true
LOCAL_STOP_INTERIM_WINDOW_MS=600
LOCAL_STOP_MIN_INTERIM_LEN=10
WORKER_LOCAL_STOP_ENABLED=true

# Audio
AUDIO_INPUT_GAIN=2.0
TTS_LLM_ACCUM_DEBOUNCE_MS=120
ORCH_FEATURE_INTERVAL_SPEAKING_SEC=0.3

# Dev mode
DEV_MODE=true
```

### Required API Keys

| Service | Purpose | Get it from |
|---------|---------|-------------|
| Daily.co | WebRTC infrastructure | https://dashboard.daily.co |
| Deepgram | Speech-to-text | https://console.deepgram.com |
| ElevenLabs | Text-to-speech | https://elevenlabs.io |
| Azure OpenAI | LLM responses | Azure Portal |

---

## 8. Generate gRPC stubs

```bash
# Activate venv if not already
source .venv/bin/activate

# Generate Go stubs
make proto-go

# Generate Python stubs
make proto-py
```

---

## 9. Build everything

```bash
go build ./...
```

---

## 10. Run the full system

```bash
make full-e2e
```

This will:
1. Start STT Sidecar (speech-to-text)
2. Start Orchestrator (conversation flow)
3. Start LLM Service (AI responses)
4. Start TTS Service (text-to-speech)
5. Start API Server (HTTP endpoints)
6. Create a Daily.co room
7. Open your browser to join the voice call

---

## Troubleshooting

### Check service health

```bash
make health-all
```

### View logs

```bash
# All logs
make diag-logs

# Specific service
make diag-orch    # Orchestrator
make diag-stt     # STT Sidecar
```

### Check what's listening on ports

```bash
make diag-status
```

### Test individual APIs

```bash
make test-deepgram        # Test Deepgram STT
make test-azure-openai    # Test Azure OpenAI
make test-elevenlabs      # Test ElevenLabs TTS
```

### Clear logs

```bash
make diag-clear
```

---

## Ports Used

| Service | gRPC Port | Metrics Port |
|---------|-----------|--------------|
| API Server | - | 8080 |
| STT Sidecar | Unix socket | 8081 |
| Orchestrator | 9090 | 8082 |
| LLM Service | 9092 | 8083 |
| TTS Service | 9093 | 8084 |

---

## Architecture

```
Browser (Daily.co WebRTC)
    │
    ▼
┌─────────────────┐
│   API Server    │ :8080
│  (Go, HTTP)     │
└────────┬────────┘
         │ spawns
         ▼
┌─────────────────┐      ┌─────────────────┐
│  Gateway/Bot    │◄────►│  Orchestrator   │ :9090
│  (Python)       │      │  (Go, gRPC)     │
└────────┬────────┘      └────────┬────────┘
         │                        │
         ▼                        ▼
┌─────────────────┐      ┌─────────────────┐
│  STT Sidecar    │      │   LLM Service   │ :9092
│  (Go, gRPC)     │      │   (Go, gRPC)    │
└────────┬────────┘      └─────────────────┘
         │                        │
         ▼                        ▼
    Deepgram API            Azure OpenAI
                                  │
                                  ▼
                         ┌─────────────────┐
                         │   TTS Service   │ :9093
                         │   (Go, gRPC)    │
                         └────────┬────────┘
                                  │
                                  ▼
                            ElevenLabs API
```

---

## Common Issues

### "STT Sidecar failed to start"

Check the log:
```bash
cat /tmp/stt-sidecar.log
```

Usually a compilation error. Run:
```bash
go build ./cmd/stt-sidecar
```

### "No audio / Can't hear bot"

1. Check browser microphone permissions
2. Ensure Daily.co room was created successfully
3. Check TTS service logs: `cat /tmp/tts.log`

### "Bot doesn't respond to speech"

1. Check STT is receiving audio: `grep "audio" /tmp/stt-sidecar.log`
2. Check transcripts are forwarding: `grep "FORWARDING" /tmp/stt-sidecar.log`
3. Check orchestrator receives them: `grep "TRANSCRIPT_FINAL" /tmp/orchestrator.log`

### Deepgram timeout errors (net0001)

The keepalive should prevent this. If you still see timeouts:
```bash
# Increase keepalive frequency
export STT_KEEPALIVE_MS=300
```

---

## Development

### Run services individually

```bash
# Terminal 1: STT Sidecar
make sidecar

# Terminal 2: Orchestrator
make orch

# Terminal 3: LLM Service
make llm

# Terminal 4: TTS Service
make tts

# Terminal 5: API Server
make server
```

### Regenerate protos after changes

```bash
make proto-go && make proto-py
```

### Run tests

```bash
make test
```
