#!/usr/bin/env python3
import os
import sys
import time
import json
import struct

def log(msg, **kwargs):
    payload = {"msg": msg}
    payload.update(kwargs)
    print(payload.get("msg"), flush=True)
    # If you want structured JSON logs later, adjust here.

def eprint(*args, **kwargs):
    print(*args, file=sys.stderr, **kwargs)

def required_env(name):
    val = os.environ.get(name, "")
    if not val:
        raise RuntimeError(f"missing env: {name}")
    return val

def fetch_tts_wav(eleven_api_key, voice_id, text):
    import requests
    url = f"https://api.elevenlabs.io/v1/text-to-speech/{voice_id}"
    headers = {
        "xi-api-key": eleven_api_key,
        "accept": "audio/wav",
        "content-type": "application/json",
    }
    data = {"text": text}
    resp = requests.post(url, headers=headers, data=json.dumps(data), timeout=30)
    resp.raise_for_status()
    return resp.content

def decode_wav_pcm16(wav_bytes):
    import io
    import wave
    import numpy as np
    bio = io.BytesIO(wav_bytes)
    with wave.open(bio, 'rb') as wf:
        n_channels = wf.getnchannels()
        sampwidth = wf.getsampwidth()
        framerate = wf.getframerate()
        n_frames = wf.getnframes()
        raw = wf.readframes(n_frames)
    if sampwidth != 2:
        raise RuntimeError(f"expected 16-bit PCM, got {sampwidth*8}-bit")
    # int16 PCM
    pcm = np.frombuffer(raw, dtype=np.int16)
    if n_channels == 2:
        pcm = pcm.reshape(-1, 2).mean(axis=1).astype(np.int16)
    return pcm, framerate, n_channels

def resample_to_48k(pcm_int16, sr):
    import numpy as np
    target = 48000
    if sr == target:
        return pcm_int16
    x = pcm_int16.astype(np.float32) / 32768.0
    old_len = len(x)
    new_len = int(round(old_len * target / sr))
    old_idx = np.linspace(0.0, 1.0, old_len, endpoint=False)
    new_idx = np.linspace(0.0, 1.0, new_len, endpoint=False)
    y = np.interp(new_idx, old_idx, x)
    y_int16 = np.clip(y * 32768.0, -32768, 32767).astype(np.int16)
    return y_int16

def pace_and_send_audio_pcm16(transport, pcm16_bytes, sr):
    # 20ms frames for 16-bit mono PCM
    bytes_per_sample = 2
    samples_per_frame = int(sr * 0.02)
    bytes_per_frame = samples_per_frame * bytes_per_sample
    sent_frames = 0
    pos = 0
    started_ms = int(time.time() * 1000)
    method = 'unknown'
    while pos < len(pcm16_bytes):
        chunk = pcm16_bytes[pos:pos+bytes_per_frame]
        pos += bytes_per_frame
        try:
            if hasattr(transport, 'send_audio_pcm16'):
                transport.send_audio_pcm16(chunk, sample_rate=sr)
                method = 'send_audio_pcm16'
            elif hasattr(transport, 'send_audio'):
                transport.send_audio(chunk, sample_rate=sr)
                method = 'send_audio'
            else:
                raise RuntimeError('transport has no audio send method')
            sent_frames += 1
        except Exception as e:
            raise
        time.sleep(0.02)
    duration_ms = int(time.time() * 1000) - started_ms
    log(f"AUDIO_FORMAT sr={sr} channels=1 width=2 frame_ms=20 samples_per_frame={samples_per_frame}")
    log(f"PUBLISHED_AUDIO_FRAMES={sent_frames}")
    log(f"AUDIO_SENT_MS={duration_ms}")
    log(f"TRANSPORT_METHOD={method}")

def try_join_daily(room_url, token):
    # Try Pipecat DailyTransport
    try:
        from pipecat.transports.daily import DailyTransport
    except Exception as e:
        log("BOT_ERROR {\"stage\":\"import\",\"error\":\"pipecat import failed\"}")
        raise
    t = DailyTransport(room_url=room_url, token=token)
    try:
        t.connect()
    except Exception as e:
        log("BOT_ERROR {\"stage\":\"join\",\"error\":\"connect failed\"}")
        raise
    return t

def main():
    room_url = required_env("DAILY_ROOM_URL")
    token = required_env("DAILY_TOKEN")
    eleven_api_key = required_env("ELEVENLABS_API_KEY")
    voice_id = required_env("ELEVENLABS_VOICE_ID")
    phrase = os.environ.get("ELEVENLABS_CANNED_PHRASE", "Hi, I'm your AI interviewer. Can you hear me clearly?")
    stay_s = int(os.environ.get("BOT_STAY_CONNECTED_SECONDS", "30"))

    log("BOT_JOINING")
    try:
        transport = try_join_daily(room_url, token)
        log("BOT_JOINED")
    except Exception as e:
        eprint("join failed:", e)
        log("BOT_EXIT")
        sys.exit(1)

    # TTS
    log("TTS_START")
    try:
        wav_bytes = fetch_tts_wav(eleven_api_key, voice_id, phrase)
        pcm_arr, sr_in, ch = decode_wav_pcm16(wav_bytes)
        # Force mono 48k PCM16
        pcm_arr_48k = resample_to_48k(pcm_arr, sr_in)
        pcm16_bytes = pcm_arr_48k.tobytes()
        log("TTS_DONE")
        try:
            pace_and_send_audio_pcm16(transport, pcm16_bytes, 48000)
        except Exception as e:
            eprint("publish error:", e)
            log("BOT_ERROR {\"stage\":\"publish\",\"error\":\"send failed\"}")
            log("BOT_EXIT")
            sys.exit(1)
    except Exception as e:
        eprint("tts error:", e)
        log("BOT_ERROR {\"stage\":\"tts\",\"error\":\"tts failed\"}")
        log("BOT_EXIT")
        sys.exit(1)

    # Stay connected for N seconds
    for i in range(stay_s, 0, -1):
        log(f"BOT_SLEEPING {i}")
        time.sleep(1)

    log("BOT_EXIT")

if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        eprint("fatal:", e)
        print("BOT_EXIT", flush=True)
        sys.exit(1)
