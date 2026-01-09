#!/usr/bin/env python3
import os
import sys
import time
import json
import struct
import asyncio
import urllib.parse

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
    # Optional WS connect
    ws_url = os.environ.get("WS_URL", "")
    worker_token = os.environ.get("WORKER_TOKEN", "")
    session_id = None
    if ws_url:
        try:
            qs = urllib.parse.urlparse(ws_url).query
            session_id = urllib.parse.parse_qs(qs).get("session_id", [None])[0]
        except Exception:
            session_id = None
    ws_task = None
    ws_queue = asyncio.Queue()
    stop_flag = {"stop": False}

    async def ws_client():
        if not ws_url:
            return
        import websockets
        headers = {}
        if worker_token:
            headers["Authorization"] = f"Bearer {worker_token}"
        async with websockets.connect(ws_url, extra_headers=headers) as ws:
            seq = 1
            # worker_hello
            hello = {"type":"worker_hello","ts_ms":int(time.time()*1000),"session_id":session_id or "","seq":seq,"payload":{"version":"p1","transport":"pipecat","audio_format":"pcm16_48k_mono"}}
            seq += 1
            await ws.send(json.dumps(hello))
            # Reader loop
            async def reader():
                async for raw in ws:
                    try:
                        msg = json.loads(raw)
                    except Exception:
                        continue
                    if msg.get("type") == "stop_tts":
                        # signal playback stop
                        stop_flag["stop"] = True
                        # ack
                        ack = {"type":"cmd_ack","ts_ms":int(time.time()*1000),"session_id":session_id or "","seq":seq,"command_id":msg.get("command_id"),"payload":{"ack":True,"error":""}}
                        seq += 1
                        await ws.send(json.dumps(ack))
            # Writer loop for events from playback
            async def writer():
                while True:
                    e = await ws_queue.get()
                    e["seq"] = seq
                    seq += 1
                    await ws.send(json.dumps(e))
            await asyncio.gather(reader(), writer())

    if ws_url:
        try:
            loop = asyncio.get_event_loop()
        except RuntimeError:
            loop = asyncio.new_event_loop()
            asyncio.set_event_loop(loop)
        ws_task = loop.create_task(ws_client())
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
        # WS: tts_started
        utterance_id = f"u-{int(time.time()*1000)}"
        if ws_url:
            evt = {"type":"tts_started","ts_ms":int(time.time()*1000),"session_id":session_id or "","command_id":"","utterance_id":utterance_id,"payload":{"source":"worker_local","text_chars":len(phrase)}}
            loop.create_task(ws_queue.put(evt))
        try:
            # paced playback with stop support
            bytes_per_sample = 2
            samples_per_frame = int(48000 * 0.02)
            bytes_per_frame = samples_per_frame * bytes_per_sample
            pos = 0
            sent_frames = 0
            started_ms = int(time.time()*1000)
            while pos < len(pcm16_bytes):
                if stop_flag["stop"]:
                    break
                chunk = pcm16_bytes[pos:pos+bytes_per_frame]
                pos += bytes_per_frame
                if hasattr(transport, 'send_audio_pcm16'):
                    transport.send_audio_pcm16(chunk, sample_rate=48000)
                else:
                    transport.send_audio(chunk, sample_rate=48000)
                sent_frames += 1
                time.sleep(0.02)
            duration_ms = int(time.time()*1000) - started_ms
            log(f"AUDIO_FORMAT sr=48000 channels=1 width=2 frame_ms=20 samples_per_frame={samples_per_frame}")
            log(f"PUBLISHED_AUDIO_FRAMES={sent_frames}")
            log(f"AUDIO_SENT_MS={duration_ms}")
            log(f"TRANSPORT_METHOD={'send_audio_pcm16' if hasattr(transport,'send_audio_pcm16') else 'send_audio'}")
            # WS: tts_stopped
            reason = "completed" if not stop_flag["stop"] else "interrupted"
            if ws_url:
                evt = {"type":"tts_stopped","ts_ms":int(time.time()*1000),"session_id":session_id or "","utterance_id":utterance_id,"payload":{"source":"worker_local","reason":reason}}
                loop.create_task(ws_queue.put(evt))
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
