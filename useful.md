# Useful: Running Services Individually

individual Make targets for each service:

- `make sidecar`  – STT Sidecar (HTTP probes on `:8081`, gRPC on UDS `/tmp/stt.sock` or `STT_UDS_PATH`)
- `make orch`     – Orchestrator (HTTP probes on `:8082`, gRPC on `:9090`)
- `make llm`      – LLM service (HTTP probes on `:8083`, gRPC on `:9092`)
- `make tts`      – TTS service (HTTP probes on `:8084`, gRPC on `:9093`)

Run each in a separate terminal, then test with:

```bash
curl http://localhost:8081/healthz  # sidecar
curl http://localhost:8082/healthz  # orchestrator
curl http://localhost:8083/healthz  # llm
curl http://localhost:8084/healthz  # tts
```

This is useful for debugging — you can see each service's logs separately and restart just the one you're working on.

---

## Testing Deepgram API

Test audio files are in `testdata/`:
- `testdata/test.wav` — 1 second of silence (connectivity test)
- `testdata/speech.wav` — LibriSpeech sample with real speech

### Quick connectivity test (silence)

```bash
make test-deepgram
```

Expected response:
- `"duration": 1.0` — audio was processed
- `"transcript": ""` — empty (no speech in silence)
- No auth errors — API key is valid

### Test with real speech

```bash
make test-deepgram-speech
```

Expected output:
```
but buy and by they came to my watch which i'd had hidden away in the in most pocket that i had and had forgotten when they began their search
```

### Adding more test audio

To add your own test files to `testdata/`:

```bash
# Record from microphone (3 seconds)
ffmpeg -f avfoundation -i ":0" -t 3 -ar 16000 -ac 1 testdata/my-recording.wav

# Convert from LibriSpeech FLAC
ffmpeg -i LibriSpeech/dev-clean/2412/153954/2412-153954-0019.flac \
  -ac 1 -ar 16000 -y testdata/speech.wav

# Convert any audio file
ffmpeg -i input.mp3 -ar 16000 -ac 1 testdata/converted.wav
```

Test custom files manually:
```bash
curl -s -X POST "https://api.deepgram.com/v1/listen" \
  -H "Authorization: Token $(grep '^DEEPGRAM_API_KEY=' .env | cut -d= -f2 | tr -d '\"')" \
  -H "Content-Type: audio/wav" \
  --data-binary @testdata/my-recording.wav | jq -r \
  '.results.channels[0].alternatives[0].transcript'
```

---

## Testing Azure OpenAI (LLM)

```bash
make test-azure-openai
```

Expected output: A greeting (e.g., "Hello! How can I assist you?")

### Environment variables used

| Variable | Description |
|----------|-------------|
| `AZURE_OPENAI_ENDPOINT` | Azure endpoint URL (e.g., `https://xxx.openai.azure.com/`) |
| `AZURE_OPENAI_API_KEY` | Azure API key |
| `AZURE_OPENAI_DEPLOYMENT` | Model deployment name (e.g., `gpt-4o-mini`) |
| `AZURE_OPENAI_API_VERSION` | API version (e.g., `2024-02-15-preview`) |

### Manual test

```bash
curl -s -X POST \
  "$AZURE_OPENAI_ENDPOINT/openai/deployments/$AZURE_OPENAI_DEPLOYMENT/chat/completions?api-version=$AZURE_OPENAI_API_VERSION" \
  -H "api-key: $AZURE_OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [{"role": "user", "content": "Say hello"}],
    "max_tokens": 50
  }' | jq .choices[0].message.content
```

---

## Testing ElevenLabs (TTS)

```bash
make test-elevenlabs
```

Expected output: Saves audio to `testdata/tts-test.mp3` and prints the file size.

### Environment variables used

| Variable | Description |
|----------|-------------|
| `ELEVENLABS_API_KEY` | ElevenLabs API key |
| `ELEVENLABS_VOICE_ID` | Voice ID to use for synthesis |

### Play the test audio

```bash
# macOS
afplay testdata/tts-test.mp3

# Linux
mpv testdata/tts-test.mp3
```

---

## Testing LLM gRPC Service

Tests the Go LLM service running on `:9092` (not Azure directly).

### Prerequisites

```bash
brew install grpcurl
```

### Test command

```bash
# First, start the LLM service
make llm

# In another terminal
make test-llm-service
```

Expected output: Streamed responses with `sentence` messages containing the LLM reply.

```json
{
  "sentence": {
    "text": "Hello! How can I help?"
  }
}
```

### Manual test with grpcurl

```bash
echo '{"start":{"session_id":"test","request_id":"test-1","deployment":"gpt-4o-mini","api_version":"2024-02-15-preview","messages":[{"role":"user","content":"Say hello"}],"stream":true}}' | \
grpcurl -plaintext -d @ -import-path proto -proto llm.proto localhost:9092 llm.v1.LLM/Session
```

