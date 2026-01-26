#!/usr/bin/env python3
import os
import sys
import time
import json
import asyncio
import urllib.parse
import webrtcvad
import numpy as np
import contextlib
import threading
import daily


def log(msg, **kwargs):
    payload = {"msg": msg}
    payload.update(kwargs)
    print(payload.get("msg"), flush=True)


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
    bio = io.BytesIO(wav_bytes)
    with wave.open(bio, 'rb') as wf:
        n_channels = wf.getnchannels()
        sampwidth = wf.getsampwidth()
        framerate = wf.getframerate()
        n_frames = wf.getnframes()
        raw = wf.readframes(n_frames)
    if sampwidth != 2:
        raise RuntimeError(f"expected 16-bit PCM, got {sampwidth*8}-bit")
    pcm = np.frombuffer(raw, dtype=np.int16)
    if n_channels == 2:
        pcm = pcm.reshape(-1, 2).mean(axis=1).astype(np.int16)
    return pcm, framerate, n_channels


def resample_to_48k(pcm_int16, sr):
    """Resample PCM16 audio to 48kHz using high-quality polyphase resampling."""
    from scipy.signal import resample_poly
    from math import gcd
    target = 48000
    if sr == target:
        return pcm_int16
    # Use polyphase resampling for better quality
    # resample_poly(x, up, down) where up/down = target/sr simplified
    g = gcd(target, sr)
    up = target // g
    down = sr // g
    # Convert to float, resample, convert back
    x = pcm_int16.astype(np.float64)
    y = resample_poly(x, up, down)
    y_int16 = np.clip(y, -32768, 32767).astype(np.int16)
    return y_int16


class VADState:
    def __init__(self, aggressiveness=2, frame_ms=20, hangover_ms=400):
        self.vad = webrtcvad.Vad(aggressiveness)
        self.frame_ms = frame_ms
        self.hangover_frames = int(hangover_ms / frame_ms)
        self.speaking = False
        self.consec_speech = 0
        self.non_speech = 0
        self.started_at_ms = 0
        self.min_start_frames = 2  # default ~40ms
        self.min_burst_frames = 6  # ~120ms

    def process_frame(self, pcm16_bytes, sample_rate):
        try:
            is_speech = self.vad.is_speech(pcm16_bytes, sample_rate)
        except Exception:
            is_speech = False
        now_ms = int(time.time() * 1000)
        if not self.speaking:
            if is_speech:
                self.consec_speech += 1
                if self.consec_speech >= self.min_start_frames:
                    self.speaking = True
                    self.started_at_ms = now_ms
                    self.non_speech = 0
                    return 'start', now_ms
            else:
                self.consec_speech = 0
            return None, None
        else:
            if is_speech:
                self.non_speech = 0
                return None, None
            else:
                self.non_speech += 1
                if self.non_speech >= self.hangover_frames:
                    dur_frames = int((now_ms - self.started_at_ms) / self.frame_ms)
                    self.speaking = False
                    self.consec_speech = 0
                    self.non_speech = 0
                    if dur_frames < self.min_burst_frames:
                        return None, None
                    return 'end', now_ms
                return None, None


def attach_candidate_vad(transport, ws_queue, session_id, loop, vad, stop_event, state):
    """Register a candidate-audio VAD callback; bridge to asyncio with call_soon_threadsafe."""
    frame_bytes = int(48000 * 0.02) * 2  # 20ms @48k, 16-bit mono
    buf = bytearray()
    frame_count = [0]  # Use list for nonlocal mutation in nested function

    def handle_frame(pcm_bytes, sample_rate=48000, channels=1):
        nonlocal buf
        frame_count[0] += 1
        # Log every 500 frames (~10 seconds) to confirm we're receiving audio
        if frame_count[0] == 1:
            print(f"REMOTE_AUDIO_RECEIVING first_frame len={len(pcm_bytes)} sr={sample_rate} ch={channels}", flush=True)
        elif frame_count[0] % 500 == 0:
            print(f"REMOTE_AUDIO_FRAMES_RECEIVED={frame_count[0]}", flush=True)
        # Decode remote audio frames as PCM16 (Daily VirtualSpeaker yields int16)
        pcm_arr = np.frombuffer(pcm_bytes, dtype=np.int16)
        if channels == 2 and pcm_arr.size % 2 == 0:
            pcm_arr = pcm_arr.reshape(-1, 2).mean(axis=1).astype(np.int16)
        if sample_rate != 48000 and pcm_arr.size > 0:
            pcm_arr = resample_to_48k(pcm_arr, sample_rate)
        b = pcm_arr.tobytes()
        buf.extend(b)
        while len(buf) >= frame_bytes:
            frame = bytes(buf[:frame_bytes])
            del buf[:frame_bytes]
            ev, ts = vad.process_frame(frame, 48000)
            if ev == 'start':
                evt = {"type": "vad_start", "ts_ms": ts, "session_id": session_id or "", "payload": {"source": "candidate_audio"}}
                loop.call_soon_threadsafe(ws_queue.put_nowait, evt)
                # Local-stop: if speaking, trigger immediate stop
                state['last_vad_ts_ms'] = ts
                # Energy gate and guard time
                try:
                    frm_arr = np.frombuffer(frame, dtype=np.int16)
                    rms = float(np.sqrt(np.mean((frm_arr.astype(np.float64))**2))) if frm_arr.size > 0 else 0.0
                except Exception:
                    rms = 0.0
                guard_ok = False
                try:
                    armed_ts = int(state.get('speaking_armed_ts_ms', 0) or 0)
                    guard_ms = int(state.get('local_stop_guard_ms', 0) or 0)
                    now_ms = int(time.time() * 1000)
                    guard_ok = (armed_ts > 0) and (now_ms - armed_ts >= guard_ms)
                except Exception:
                    guard_ok = True
                min_rms = float(state.get('local_stop_min_rms', 0) or 0)
                if state.get('local_stop_enabled', True) and state.get('speaking_armed', False) and guard_ok and rms >= min_rms:
                    # Schedule stop on the asyncio loop thread; thread-safe
                    try:
                        loop.call_soon_threadsafe(stop_event.set)
                    except Exception:
                        # Fallback (non-thread-safe) in case loop reference is invalid
                        try:
                            stop_event.set()
                        except Exception:
                            pass
            elif ev == 'end':
                evt = {"type": "vad_end", "ts_ms": ts, "session_id": session_id or "", "payload": {"source": "candidate_audio"}}
                loop.call_soon_threadsafe(ws_queue.put_nowait, evt)

    # Try to register callback on transport
    if hasattr(transport, 'on_remote_audio'):
        try:
            transport.on_remote_audio = handle_frame
            print("CANDIDATE_AUDIO_FRAMES hook=on_remote_audio", flush=True)
            return
        except Exception as e:
            eprint("hook on_remote_audio failed:", e)
    if hasattr(transport, 'add_remote_audio_callback'):
        try:
            transport.add_remote_audio_callback(handle_frame)
            print("CANDIDATE_AUDIO_FRAMES hook=add_remote_audio_callback", flush=True)
            return
        except Exception as e:
            eprint("hook add_remote_audio_callback failed:", e)
    print("CANDIDATE_AUDIO_RX_UNAVAILABLE", flush=True)


async def playback_task(transport, pcm16_bytes, sr, stop_event, loop, ws_queue, session_id, utterance_id, state):
    """Send audio in 20ms frames with early-wake stop."""
    bytes_per_sample = 2
    samples_per_frame = int(sr * 0.02)
    bytes_per_frame = samples_per_frame * bytes_per_sample
    pos = 0
    sent_frames = 0
    started_ms = int(time.time() * 1000)
    method = 'unknown'

    while pos < len(pcm16_bytes):
        if stop_event.is_set():
            break
        chunk = pcm16_bytes[pos:pos + bytes_per_frame]
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
            if sent_frames == 1:
                state['speaking_armed'] = True
                state['speaking_armed_ts_ms'] = int(time.time() * 1000)
        except Exception as e:
            eprint("publish error:", e)
            raise
        # Early-wake wait for ~20ms or until stop
        try:
            await asyncio.wait_for(stop_event.wait(), timeout=0.02)
        except asyncio.TimeoutError:
            pass

    duration_ms = int(time.time() * 1000) - started_ms
    log(f"AUDIO_FORMAT sr={sr} channels=1 width=2 frame_ms=20 samples_per_frame={samples_per_frame}")
    log(f"PUBLISHED_AUDIO_FRAMES={sent_frames}")
    log(f"AUDIO_SENT_MS={duration_ms}")
    log(f"TRANSPORT_METHOD={method}")

    # WS: tts_stopped (reason determined by stop_event)
    reason = "interrupted" if stop_event.is_set() else "completed"
    if session_id and not state.get('tts_stop_emitted', False):
        now_ts = int(time.time() * 1000)
        payload = {"source": "worker_local", "reason": reason}
        vad_ts = state.get('last_vad_ts_ms')
        if reason == 'interrupted' and isinstance(vad_ts, (int, float)) and vad_ts > 0:
            payload['barge_in_ms'] = max(0, now_ts - int(vad_ts))
        evt = {"type": "tts_stopped", "ts_ms": now_ts, "session_id": session_id, "utterance_id": utterance_id, "payload": payload}
        state['tts_stop_emitted'] = True
        await ws_queue.put(evt)
    # Disarm after playback completes
    state['speaking_armed'] = False


class WavStreamParser:
    """Minimal streaming WAV parser for PCM16. Produces raw PCM16 bytes.
    Assumes little-endian PCM. Converts stereo to mono by averaging.
    """
    def __init__(self):
        self.buf = bytearray()
        self.header_parsed = False
        self.expect_data = False
        self.channels = 1
        self.sample_rate = 48000
        self.bits_per_sample = 16
        self._cursor = 0
        self._data_remaining = None

    def feed(self, data: bytes):
        self.buf.extend(data)

    def _read_u32le(self, off):
        b = self.buf[off:off+4]
        return int.from_bytes(b, 'little')

    def _read_u16le(self, off):
        b = self.buf[off:off+2]
        return int.from_bytes(b, 'little')

    def parse_header(self):
        if self.header_parsed:
            return True
        if len(self.buf) < 12:
            return False
        if self.buf[0:4] != b'RIFF' or self.buf[8:12] != b'WAVE':
            raise RuntimeError('not a WAV stream')
        # iterate chunks
        off = 12
        while True:
            if len(self.buf) < off + 8:
                return False
            cid = bytes(self.buf[off:off+4])
            csz = self._read_u32le(off+4)
            off += 8
            if cid == b'fmt ':
                if len(self.buf) < off + csz:
                    return False
                fmt_tag = self._read_u16le(off)
                self.channels = self._read_u16le(off+2)
                self.sample_rate = self._read_u32le(off+4)
                # byte rate at off+8, block align at off+12
                self.bits_per_sample = self._read_u16le(off+14)
                if fmt_tag != 1 or self.bits_per_sample != 16:
                    raise RuntimeError('unsupported WAV format (need PCM16)')
                off += csz
            elif cid == b'data':
                # ready to stream data
                self.header_parsed = True
                self.expect_data = True
                # Trim header bytes
                self.buf = self.buf[off:]
                self._cursor = 0
                return True
            else:
                # skip other chunks
                if len(self.buf) < off + csz:
                    return False
                off += csz
        
    def read_pcm_bytes(self, max_bytes=None):
        if not self.header_parsed:
            return b''
        if max_bytes is None:
            max_bytes = len(self.buf)
        out = bytes(self.buf[:max_bytes])
        del self.buf[:max_bytes]
        return out


def pcm_stereo_to_mono(pcm: np.ndarray) -> np.ndarray:
    if pcm.ndim == 1:
        return pcm
    return pcm.reshape(-1, 2).mean(axis=1).astype(np.int16)


def slice_frames(pcm16_bytes: bytearray, frame_bytes: int):
    while len(pcm16_bytes) >= frame_bytes:
        chunk = bytes(pcm16_bytes[:frame_bytes])
        del pcm16_bytes[:frame_bytes]
        yield chunk


def _producer_stream_elevenlabs(eleven_api_key, voice_id, text, loop, queue, stop_flag: threading.Event, metrics: dict):
    """Blocking producer: streams raw PCM from ElevenLabs and pushes 20ms PCM16@48k frames via the loop to an asyncio.Queue with backpressure."""
    import requests
    # Use native 48kHz PCM format - no resampling needed
    pcm_sample_rate = 48000
    url = f"https://api.elevenlabs.io/v1/text-to-speech/{voice_id}/stream?output_format=pcm_48000"
    headers = {
        "xi-api-key": eleven_api_key,
        "content-type": "application/json",
    }
    data = {"text": text}
    frame_bytes_48k = int(48000 * 0.02) * 2  # 20ms @ 48kHz, 16-bit = 1920 bytes
    out_buf = bytearray()
    raw_buf = bytearray()  # Buffer for unaligned incoming bytes
    log("DEBUG: producer HTTP request starting")
    chunk_count = 0
    try:
        with requests.post(url, headers=headers, data=json.dumps(data), stream=True, timeout=30) as resp:
            log(f"DEBUG: producer HTTP response status={resp.status_code}")
            resp.raise_for_status()
            for chunk in resp.iter_content(chunk_size=4096):
                chunk_count += 1
                if chunk_count == 1:
                    log(f"DEBUG: producer first chunk received, len={len(chunk)}")
                if stop_flag.is_set():
                    log("DEBUG: producer stop_flag set, breaking")
                    break
                if not chunk:
                    continue
                # Buffer raw bytes to ensure 2-byte alignment for int16
                raw_buf.extend(chunk)
                aligned_len = (len(raw_buf) // 2) * 2
                if aligned_len == 0:
                    continue
                aligned_bytes = bytes(raw_buf[:aligned_len])
                del raw_buf[:aligned_len]
                # Native 48kHz PCM16 - no resampling needed
                out_buf.extend(aligned_bytes)
                # Emit complete 20ms frames with backpressure
                for frm in slice_frames(out_buf, frame_bytes_48k):
                    if metrics.get('first_frame_sent_ts_ms') is None:
                        metrics['first_frame_sent_ts_ms'] = int(time.time() * 1000)
                        log("DEBUG: first frame queued")
                    fut = asyncio.run_coroutine_threadsafe(queue.put(frm), loop)
                    try:
                        fut.result()  # backpressure
                    except Exception as e:
                        log(f"DEBUG: queue put failed: {e}")
                        return
            log("DEBUG: producer HTTP stream finished")
            # Send sentinel to signal completion
            fut = asyncio.run_coroutine_threadsafe(queue.put(None), loop)
            try:
                fut.result()
            except Exception:
                pass
    except Exception as e:
        log(f"DEBUG: producer exception: {e}")
        # Send sentinel even on error
        try:
            fut = asyncio.run_coroutine_threadsafe(queue.put(None), loop)
            fut.result()
        except Exception:
            pass


async def tts_streaming_play(loop, transport, eleven_api_key, voice_id, text, stop_event, ws_queue, session_id, utterance_id, state):
    """Streaming TTS end-to-end: producer + consumer with prebuffer and underrun handling."""
    log("DEBUG: tts_streaming_play started")
    queue = asyncio.Queue(maxsize=25)  # ~500ms at 20ms frames
    frame_bytes = int(48000 * 0.02) * 2
    metrics = {'first_frame_sent_ts_ms': None, 'queue_peak_frames': 0}
    stop_flag = threading.Event()

    def start_producer():
        log("DEBUG: producer starting")
        _producer_stream_elevenlabs(eleven_api_key, voice_id, text, loop, queue, stop_flag, metrics)
        log("DEBUG: producer finished")

    # Start producer in threadpool
    log("DEBUG: launching producer in threadpool")
    prod_fut = loop.run_in_executor(None, start_producer)

    # Prebuffer 10-25 frames (200-500ms) to smooth network jitter
    prebuffer_target = int(os.environ.get('TTS_PREBUFFER_FRAMES', '15'))
    prebuffer_target = max(10, min(25, prebuffer_target))
    prebuffer_timeout_secs = int(os.environ.get('TTS_PREBUFFER_TIMEOUT_SECS', '30'))
    log(f"DEBUG: waiting for prebuffer, target={prebuffer_target}, timeout={prebuffer_timeout_secs}s")
    prebuffer_timeout = time.time() + prebuffer_timeout_secs
    while queue.qsize() < prebuffer_target and not stop_event.is_set():
        if time.time() > prebuffer_timeout:
            log("DEBUG: prebuffer timeout")
            break
        # Check if producer finished early (peek for None sentinel)
        if not prod_fut.done():
            await asyncio.sleep(0.01)
        else:
            log("DEBUG: producer finished during prebuffer")
            break
        metrics['queue_peak_frames'] = max(metrics['queue_peak_frames'], queue.qsize())
    log(f"DEBUG: prebuffer done, queue size={queue.qsize()}")

    # Consumer loop with monotonic timing for consistent frame rate
    sent_frames = 0
    first_audio_emitted = False
    completed_normally = False
    frame_duration = 0.02  # 20ms per frame
    next_frame_time = time.monotonic()

    try:
        while not stop_event.is_set():
            try:
                frm = await asyncio.wait_for(queue.get(), timeout=0.5)
            except asyncio.TimeoutError:
                # True underrun - no data for 500ms during streaming
                log("DEBUG: consumer timeout - buffer underrun")
                reason = 'buffer_underrun'
                if session_id and not state.get('tts_stop_emitted', False):
                    now_ts = int(time.time() * 1000)
                    payload = {"source": "worker_local", "reason": reason}
                    vad_ts = state.get('last_vad_ts_ms')
                    if isinstance(vad_ts, (int, float)) and vad_ts > 0:
                        payload['barge_in_ms'] = max(0, now_ts - int(vad_ts))
                    evt = {"type": "tts_stopped", "ts_ms": now_ts, "session_id": session_id, "utterance_id": utterance_id, "payload": payload}
                    state['tts_stop_emitted'] = True
                    await ws_queue.put(evt)
                break
            # Check for sentinel (None) indicating producer is done
            if frm is None:
                log("DEBUG: consumer received sentinel - stream complete")
                completed_normally = True
                break

            # Wait until it's time to send this frame (monotonic timing)
            now = time.monotonic()
            sleep_time = next_frame_time - now
            if sleep_time > 0:
                # Use asyncio.sleep for better event loop cooperation
                # but check stop_event periodically for responsiveness
                if sleep_time > 0.005:  # Only sleep if > 5ms remaining
                    try:
                        await asyncio.wait_for(stop_event.wait(), timeout=sleep_time)
                        break  # stop_event was set
                    except asyncio.TimeoutError:
                        pass

            # Send frame
            try:
                if hasattr(transport, 'send_audio_pcm16'):
                    transport.send_audio_pcm16(frm, sample_rate=48000)
                else:
                    transport.send_audio(frm, sample_rate=48000)
                sent_frames += 1
                if sent_frames == 1:
                    log(f"DEBUG: first frame sent to transport, len={len(frm)}")
                    state['speaking_armed'] = True
                    state['speaking_armed_ts_ms'] = int(time.time() * 1000)
                if not first_audio_emitted:
                    first_audio_emitted = True
                    if session_id:
                        now_ts = int(time.time() * 1000)
                        tts_started_ts = state.get('tts_started_ts_ms', now_ts)
                        first_audio_ms = max(0, now_ts - int(tts_started_ts))
                        evt = {"type": "tts_first_audio", "ts_ms": now_ts, "session_id": session_id, "payload": {"first_audio_ms": first_audio_ms}}
                        await ws_queue.put(evt)
            except Exception as e:
                log(f"DEBUG: transport send error: {e}")
                stop_event.set()
                break

            metrics['queue_peak_frames'] = max(metrics['queue_peak_frames'], queue.qsize())
            next_frame_time += frame_duration  # Schedule next frame exactly 20ms later
        # Emit tts_stopped with appropriate reason
        if session_id and not state.get('tts_stop_emitted', False):
            now_ts = int(time.time() * 1000)
            if completed_normally:
                reason = 'completed'
            elif stop_event.is_set():
                reason = 'interrupted'
            else:
                reason = 'unknown'
            payload = {"source": "worker_local", "reason": reason}
            vad_ts = state.get('last_vad_ts_ms')
            if reason == 'interrupted' and isinstance(vad_ts, (int, float)) and vad_ts > 0:
                barge_in_ms = max(0, now_ts - int(vad_ts))
                payload['barge_in_ms'] = barge_in_ms
                log(f"*** BARGE-IN DETECTED *** latency={barge_in_ms}ms (VAD -> playback stop)")
            evt = {"type": "tts_stopped", "ts_ms": now_ts, "session_id": session_id, "utterance_id": utterance_id, "payload": payload}
            state['tts_stop_emitted'] = True
            await ws_queue.put(evt)
        log(f"DEBUG: TTS playback done, sent_frames={sent_frames}, completed_normally={completed_normally}")
    finally:
        # Cancel producer
        stop_flag.set()
        with contextlib.suppress(Exception):
            await asyncio.wait_for(prod_fut, timeout=1.0)
        # Drain queue
        try:
            while True:
                queue.get_nowait()
        except Exception:
            pass
        # Emit queue peak metric
        if session_id and metrics['queue_peak_frames'] is not None:
            now_ts = int(time.time() * 1000)
            evt = {"type": "tts_queue_peak_frames", "ts_ms": now_ts, "session_id": session_id, "payload": {"peak_frames": metrics['queue_peak_frames']}}
            with contextlib.suppress(Exception):
                await ws_queue.put(evt)
        # Disarm local-stop after playback concludes
        state['speaking_armed'] = False


class DailyTransportWrapper(daily.EventHandler):
    """Wrapper around daily-python SDK to provide a simple interface for joining rooms and sending audio."""

    def __init__(self, room_url, token):
        super().__init__()
        daily.Daily.init()
        self.client = daily.CallClient(event_handler=self)
        self.room_url = room_url
        self.token = token
        self.mic = None
        self.speaker = None
        self._joined = False
        self._remote_audio_callback = None
        self._participant_joined_flag = threading.Event()
        self._user_participant_id = None

    def connect(self):
        # Create virtual microphone for sending audio (via Daily factory)
        self.mic = daily.Daily.create_microphone_device(
            "bot-mic",
            sample_rate=48000,
            channels=1
        )
        # Create virtual speaker for receiving audio (via Daily factory)
        self.speaker = daily.Daily.create_speaker_device(
            "bot-speaker",
            sample_rate=48000,
            channels=1,
            non_blocking=False  # we will read frames in a background thread
        )
        daily.Daily.select_speaker_device("bot-speaker")

        # Join the room
        self.client.join(self.room_url, self.token, completion=self._on_joined)

        # Wait for join (simple polling)
        import time
        timeout = 10
        start = time.time()
        while not self._joined and (time.time() - start) < timeout:
            time.sleep(0.1)

        if not self._joined:
            raise RuntimeError("Failed to join Daily room within timeout")

        # Enable the virtual microphone for publishing
        # Try to disable audio processing (echo cancellation, noise suppression, auto gain)
        # to prevent muffling of clean TTS audio - this is "music mode" for the bot
        try:
            self.client.update_inputs({
                "microphone": {
                    "isEnabled": True,
                    "settings": {
                        "deviceId": "bot-mic",
                        "customConstraints": {
                            "echoCancellation": {"exact": False},
                            "noiseSuppression": {"exact": False},
                            "autoGainControl": {"exact": False}
                        }
                    }
                }
            })
            log("DAILY_MIC_ENABLED audio_processing=disabled")
        except Exception as e:
            # Fallback: enable mic without custom constraints
            eprint(f"customConstraints failed ({e}), enabling mic without them")
            self.client.update_inputs({
                "microphone": {
                    "isEnabled": True,
                    "settings": {
                        "deviceId": "bot-mic"
                    }
                }
            })
            log("DAILY_MIC_ENABLED audio_processing=default")

        # Start a background reader to pull frames from the virtual speaker
        # and forward them into the VAD path via _on_speaker_audio.
        def _speaker_reader():
            try:
                sr = getattr(self.speaker, 'sample_rate', 48000) or 48000
                ch = getattr(self.speaker, 'channels', 1) or 1
                num_frames = int(sr / 100) * 2  # 20ms
                print(f"SPEAKER_READER_STARTED sr={sr} ch={ch} frames={num_frames}", flush=True)
                while True:
                    try:
                        data = self.speaker.read_frames(num_frames)
                        if data and self._remote_audio_callback:
                            # Bridge into existing callback path
                            self._remote_audio_callback(data, sample_rate=sr, channels=ch)
                        else:
                            # Back off slightly if no data
                            time.sleep(0.005)
                    except Exception as e:
                        eprint(f"speaker read error: {e}")
                        time.sleep(0.05)
            except Exception as e:
                eprint(f"speaker reader init error: {e}")

        t = threading.Thread(target=_speaker_reader, daemon=True)
        t.start()

    def _on_joined(self, data, error):
        if error:
            log(f"JOIN_ERROR: {error}")
            return
        self._joined = True
        log("DAILY_JOINED")

    def on_participant_joined(self, participant):
        """Daily SDK event handler - called when any participant joins."""
        # Ignore the bot itself (local participant)
        if participant.get("info", {}).get("isLocal", False):
            return
        participant_id = participant.get("id", "unknown")
        log(f"PARTICIPANT_JOINED id={participant_id}")
        self._user_participant_id = participant_id
        # Subscribe to this participant's media so the speaker reader receives audio frames
        try:
            self.client.update_subscriptions(participant_settings={
                participant_id: {"media": "subscribed"}
            })
            log(f"SUBSCRIBED_MEDIA participant_id={participant_id}")
        except Exception as e:
            eprint(f"subscribe failed for {participant_id}: {e}")
        self._participant_joined_flag.set()

    def _check_existing_participants(self):
        """Check if non-local participants already exist (user joined before bot)."""
        try:
            participants = self.client.participants()
            for pid, pdata in participants.items():
                if pid == "local":
                    continue
                info = pdata.get("info", {})
                if not info.get("isLocal", False):
                    log(f"PARTICIPANT_ALREADY_PRESENT id={pid}")
                    self._user_participant_id = pid
                    # Ensure subscription for already-present participant
                    try:
                        self.client.update_subscriptions(participant_settings={
                            pid: {"media": "subscribed"}
                        })
                        log(f"SUBSCRIBED_MEDIA participant_id={pid}")
                    except Exception as e:
                        eprint(f"subscribe failed for {pid}: {e}")
                    self._participant_joined_flag.set()
                    return True
        except Exception as e:
            eprint(f"check_existing_participants error: {e}")
        return False

    def wait_for_participant(self, timeout=120):
        """Block until a non-local participant joins or timeout. Returns True if participant found."""
        # First check if someone is already in the room
        if self._check_existing_participants():
            return True
        # Wait for participant joined event
        return self._participant_joined_flag.wait(timeout=timeout)

    def _on_speaker_audio(self, audio_data):
        """Called when remote audio is received."""
        if self._remote_audio_callback and audio_data:
            try:
                # audio_data is daily.AudioData with audio_frames (bytes), sample_rate, num_channels
                self._remote_audio_callback(
                    audio_data.audio_frames,
                    sample_rate=audio_data.sample_rate,
                    channels=audio_data.num_channels
                )
            except Exception as e:
                eprint(f"remote audio callback error: {e}")

    @property
    def on_remote_audio(self):
        return self._remote_audio_callback

    @on_remote_audio.setter
    def on_remote_audio(self, callback):
        self._remote_audio_callback = callback

    def send_audio_pcm16(self, chunk, sample_rate=48000):
        """Send PCM16 audio to the room."""
        if self.mic:
            try:
                # daily-python VirtualMicrophoneDevice.write_frames takes raw bytes directly
                self.mic.write_frames(chunk)
            except Exception as e:
                eprint(f"write_frames error: {e}")
                raise

    def send_audio(self, chunk, sample_rate=48000):
        """Alias for send_audio_pcm16."""
        self.send_audio_pcm16(chunk, sample_rate)


def try_join_daily(room_url, token):
    """Join a Daily room and return a transport wrapper."""
    try:
        transport = DailyTransportWrapper(room_url, token)
        transport.connect()
        return transport
    except Exception as e:
        log(f"BOT_ERROR {{\"stage\":\"join\",\"error\":\"{str(e)}\"}}")
        raise


async def run_ws(ws_url, worker_token, session_id, ws_queue, stop_event, state):
    if not ws_url:
        return
    import websockets
    headers = {}
    if worker_token:
        headers["Authorization"] = f"Bearer {worker_token}"
    async with websockets.connect(ws_url, extra_headers=headers) as ws:
        seq = 1
        hello = {
            "type": "worker_hello",
            "ts_ms": int(time.time() * 1000),
            "session_id": session_id or "",
            "seq": seq,
            "payload": {"version": "p1", "transport": "pipecat", "audio_format": "pcm16_48k_mono", "local_stop_capable": True}
        }
        seq += 1
        await ws.send(json.dumps(hello))

        async def reader():
            nonlocal seq
            async for raw in ws:
                try:
                    msg = json.loads(raw)
                except Exception:
                    continue
                t = msg.get("type")
                if t == "stop_tts":
                    stop_event.set()
                    cmd_id = msg.get("command_id")
                    ack = {"type": "cmd_ack", "ts_ms": int(time.time() * 1000), "session_id": session_id or "", "seq": seq, "command_id": cmd_id, "payload": {"ack": True, "error": ""}}
                    seq += 1
                    await ws.send(json.dumps(ack))
                elif t == "policy":
                    try:
                        p = msg.get("payload") or {}
                        lse = bool(p.get("local_stop_enabled"))
                        state['local_stop_enabled'] = lse
                    except Exception:
                        pass

        async def writer():
            nonlocal seq
            while True:
                e = await ws_queue.get()
                e["seq"] = seq
                seq += 1
                await ws.send(json.dumps(e))

        await asyncio.gather(reader(), writer())


async def main():
    room_url = required_env("DAILY_ROOM_URL")
    token = required_env("DAILY_TOKEN")
    eleven_api_key = required_env("ELEVENLABS_API_KEY")
    voice_id = required_env("ELEVENLABS_VOICE_ID")
    phrase = os.environ.get("ELEVENLABS_CANNED_PHRASE", "Hi, I'm your AI interviewer. Can you hear me clearly?")
    stay_s = int(os.environ.get("BOT_STAY_CONNECTED_SECONDS", "30"))

    log("BOT_JOINING")

    # Optional WS wiring
    ws_url = os.environ.get("WS_URL", "")
    worker_token = os.environ.get("WORKER_TOKEN", "")
    session_id = None
    if ws_url:
        try:
            qs = urllib.parse.urlparse(ws_url).query
            session_id = urllib.parse.parse_qs(qs).get("session_id", [None])[0]
        except Exception:
            session_id = None

    ws_queue = asyncio.Queue()
    stop_event = asyncio.Event()
    # Shared worker state for local-stop logic (init early so WS policy can update it)
    state = {'speaking': False, 'active_utterance_id': '', 'last_vad_ts_ms': 0, 'tts_stop_emitted': False}
    state['local_stop_enabled'] = os.environ.get('LOCAL_STOP_ENABLED', 'true').lower() not in ('0', 'false', 'no')
    # Guard to avoid barge-in before users hear anything; default 250ms
    try:
        state['local_stop_guard_ms'] = int(os.environ.get('LOCAL_STOP_GUARD_MS', '250'))
    except Exception:
        state['local_stop_guard_ms'] = 250
    # Minimum RMS (0-32767) to accept VAD start as real speech; default 800
    try:
        state['local_stop_min_rms'] = int(os.environ.get('LOCAL_STOP_MIN_RMS', '800'))
    except Exception:
        state['local_stop_min_rms'] = 800

    ws_task = None
    if ws_url:
        ws_task = asyncio.create_task(run_ws(ws_url, worker_token, session_id, ws_queue, stop_event, state))

    # Join Daily synchronously (library is sync); run in thread to avoid blocking loop
    loop = asyncio.get_running_loop()
    try:
        transport = await loop.run_in_executor(None, try_join_daily, room_url, token)
        log("BOT_JOINED")
    except Exception as e:
        eprint("join failed:", e)
        log("BOT_EXIT")
        return

    # Wait for user to join before speaking
    participant_timeout = int(os.environ.get("BOT_PARTICIPANT_TIMEOUT_SECONDS", "120"))
    log(f"BOT_WAITING_FOR_PARTICIPANT timeout={participant_timeout}s")
    participant_found = await loop.run_in_executor(None, transport.wait_for_participant, participant_timeout)
    if not participant_found:
        log("BOT_PARTICIPANT_TIMEOUT")
        log("BOT_EXIT")
        return
    log("BOT_PARTICIPANT_READY")

    # Candidate audio VAD wiring
    vad = VADState(aggressiveness=int(os.environ.get('WORKER_VAD_AGGRESSIVENESS', '2')), frame_ms=20, hangover_ms=400)
    try:
        attach_candidate_vad(transport, ws_queue, session_id, loop, vad, stop_event, state)
    except Exception as e:
        eprint("candidate audio hook unavailable:", e)

    # Decide streaming vs non-streaming
    use_streaming = os.environ.get('ELEVENLABS_STREAMING', 'true').lower() not in ('0', 'false', 'no')
    log(f"DEBUG: use_streaming={use_streaming}")

    # Emit tts_started and mark speaking
    log("DEBUG: setting speaking state")
    speaking = True
    state['speaking'] = True
    state['speaking_armed'] = False  # only arm after first audio is sent
    # Require multiple consecutive speech frames while TTS is active to reduce false positives
    try:
        vad_start_frames_during_tts = int(os.environ.get('WORKER_VAD_MIN_START_FRAMES_WHILE_TTS', '3'))
    except Exception:
        vad_start_frames_during_tts = 3
    vad.min_start_frames = max(1, vad_start_frames_during_tts)
    utterance_id = f"u-{int(time.time()*1000)}"
    state['active_utterance_id'] = utterance_id
    state['tts_started_ts_ms'] = int(time.time() * 1000)
    if session_id:
        await ws_queue.put({
            "type": "tts_started",
            "ts_ms": state['tts_started_ts_ms'],
            "session_id": session_id,
            "command_id": "",
            "utterance_id": utterance_id,
            "payload": {"source": "worker_local", "text_chars": len(phrase), "streaming": use_streaming}
        })

    log("DEBUG: starting TTS playback")
    try:
        if use_streaming:
            log("DEBUG: using streaming TTS")
            await tts_streaming_play(loop, transport, eleven_api_key, voice_id, phrase, stop_event, ws_queue, session_id, utterance_id, state)
        else:
            # Fallback: fetch-then-play
            log("TTS_START")
            wav_bytes = await loop.run_in_executor(None, fetch_tts_wav, eleven_api_key, voice_id, phrase)
            pcm_arr, sr_in, _ = decode_wav_pcm16(wav_bytes)
            pcm_arr_48k = resample_to_48k(pcm_arr, sr_in)
            pcm16_bytes = pcm_arr_48k.tobytes()
            log("TTS_DONE")
            await playback_task(transport, pcm16_bytes, 48000, stop_event, loop, ws_queue, session_id, utterance_id, state)
    except Exception:
        log("BOT_ERROR {\"stage\":\"publish\",\"error\":\"send failed\"}")
        log("BOT_EXIT")
        return
    finally:
        speaking = False
        state['speaking'] = False
        state['active_utterance_id'] = ''
        vad.min_start_frames = 2  # restore default when not speaking
        state['tts_stop_emitted'] = False

    # Stay connected for N seconds (async)
    for i in range(stay_s, 0, -1):
        log(f"BOT_SLEEPING {i}")
        try:
            await asyncio.wait_for(stop_event.wait(), timeout=1.0)
            break
        except asyncio.TimeoutError:
            pass

    log("BOT_EXIT")

    # Cleanup WS task if running
    if ws_task:
        ws_task.cancel()
        try:
            await ws_task
        except asyncio.CancelledError:
            pass


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except Exception as e:
        eprint("fatal:", e)
        print("BOT_EXIT", flush=True)
        sys.exit(1)
