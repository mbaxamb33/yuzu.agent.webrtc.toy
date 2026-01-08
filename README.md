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