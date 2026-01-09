# Worker Protocol (Phase 2)

Envelope (all messages):

```
{
  "type": "string",
  "ts_ms": 1730000000000,
  "session_id": "uuid",
  "seq": 1,
  "command_id": "uuid-optional",
  "utterance_id": "u-optional",
  "payload": {}
}
```

Notes:
- `ts_ms`: worker clock for worker-originated events.
- `seq`: per-connection monotonic counter.
- `command_id`: only for commands and `cmd_ack`.
- `utterance_id`: optional; used for TTS events.

Worker → Backend types:
- `worker_hello` payload: `{ "version":"...", "transport":"pipecat", "audio_format":"pcm16_48k_mono" }`
- `vad_start` payload: `{ "source":"candidate_audio", "confidence":0.0-1.0 }`
- `vad_end` payload: `{ "source":"candidate_audio" }`
- `tts_started` payload: `{ "source":"worker_local", "text_chars": n }`
- `tts_stopped` payload: `{ "source":"worker_local", "reason":"completed|interrupted|error" }`
- `cmd_ack` payload: `{ "ack": true, "error": "" }`

Backend → Worker command types:
- `stop_tts` payload: `{ "mode":"current|all" }`

Semantics:
- Stop is immediate and final; ignore utterance mismatch and stop any active playback.
- Reconnect replaces the old connection.
- On connect/disconnect, backend appends `worker_connected` / `worker_disconnected`.

Auth:
- Worker connects to `GET /ws/worker?session_id=...` with `Authorization: Bearer <token>` header.
- Token format: `base64url(session_id + "." + exp_unix + "." + hex(hmac_sha256(secret, session_id+"."+exp_unix)))`.
- Backend validates signature, matches session_id, and checks `exp` (with small skew).

