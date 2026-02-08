#!/usr/bin/env python3
import os
import sys
import time
import json
import asyncio
import urllib.parse
from .gateway_control_client import GatewayControlClient
from .stt_sidecar_client import STTSidecarClient
from .audio_utils import RingBuffer, FrameBatcher, downsample_48k_to_16k
from .tts_client import TTSClient
import webrtcvad
import numpy as np
import contextlib
import threading
import daily


def _now_ts_ms():
    return int(time.time() * 1000)


def _mono_ms():
    return int(time.monotonic() * 1000)


# Log configuration
LOG_FORMAT = os.environ.get("LOG_FORMAT", "pretty")  # "pretty" or "json"
LOG_VERBOSE = os.environ.get("LOG_VERBOSE", "false").lower() not in ("0", "false", "no")
LOG_MIN_EVENTS = set((os.environ.get("LOG_MIN_EVENTS", "") or "").split(",")) if os.environ.get("LOG_MIN_EVENTS") else {
    # Core lifecycle
    "bot_joining", "daily_joined", "daily_mic_enabled", "bot_joined", "bot_exit",
    # Call/participant
    "bot_waiting_for_participant", "participant_joined", "subscribed_media", "bot_participant_ready",
    # TTS key points
    "tts_mode", "tts_started", "tts_first_audio", "tts_playback_done", "tts_timing_breakdown",
    "tts_producer_http_response", "tts_producer_first_chunk", "tts_producer_exception",
    "tts_prebuffer_done", "tts_consumer_underrun", "tts_stream_complete",
    "tts_pcm_fetched", "tts_pcm_fetch_failed",
    "tts_fetch_start", "tts_fetch_connected", "tts_fetch_eof", "tts_fetch_error", "tts_fetch_exception",
    # Barge-in
    "local_stop_triggered", "vad_start_suppressed", "barge_in_detected",
    # VAD events (important for debugging user speech detection)
    "vad_start_fired", "vad_start_detected", "vad_end_reached_hangover",
    # Orchestrator/STT
    "orchestrator_connected", "orchestrator_connect_error", "orchestrator_arm_barge_in", "orchestrator_mic_to_stt",
    "orchestrator_start_tts_received", "orchestrator_stream_closed", "orchestrator_transcript_send_error",
    "orchestrator_feature_send_failed", "orchestrator_feature_call_none",
    "orchestrator_tts_event_sent", "orchestrator_tts_event_failed", "orchestrator_tts_event_call_none",
    "orchestrator_tts_event_queued", "orchestrator_transcript_queued", "orchestrator_write_error",
    "stt_connected", "stt_error", "stt_utterance_start", "stt_audio_sent",
    "stt_transcript_final", "stt_transcript_interim",
    "stt_sending_to_orchestrator", "stt_sent_to_orchestrator", "stt_orchestrator_send_error", "stt_no_orchestrator_attached",
    # Debug
    "debug_env", "debug_orch_connecting", "debug_stt_config", "debug_stt_creating_client",
    "debug_stt_attached_orch", "debug_stt_preconnecting", "debug_stt_preconnected", "debug_stt_disabled",
    "debug_vad_start_stt_check", "debug_stt_starting_utterance", "debug_stt_start_error",
    # Audio diagnostics
    "speaker_reader_stats", "speaker_high_rms", "audio_processed_rms", "subscribe_failed",
    "remote_audio_first_frame",
    # Errors
    "stderr", "bot_error", "candidate_audio_hook_failed", "speaker_read_error", "daily_join_error",
    # Audio format detection
    "remote_audio_first_frame", "candidate_audio_hook_set",
    "speaker_reader_started", "speaker_reader_stats",
}


def _log_icon(event: str) -> str:
    """Return icon for event type."""
    if "error" in event or event == "stderr":
        return "✗"
    if "suppressed" in event:
        return "⊘"
    if event in ("local_stop_triggered", "barge_in_detected"):
        return "⚡"
    if event in ("tts_first_audio", "tts_timing_breakdown"):
        return "♪"
    if "participant" in event or "subscribed" in event:
        return "👤"
    return "▶"


def _log_summary(event: str, reason: str | None, metrics: dict | None) -> str:
    """Return concise summary of key metrics for pretty format."""
    parts = []
    if reason:
        parts.append(f"reason={reason}")
    if not metrics:
        return " ".join(parts)

    # Event-specific summaries (most important fields only)
    m = metrics
    if event == "tts_timing_breakdown":
        if m.get("first_frame_sent_ts_ms") and m.get("tts_started_ts_ms"):
            latency = m["first_frame_sent_ts_ms"] - m["tts_started_ts_ms"]
            parts.append(f"first_audio={latency}ms")
    elif event == "tts_first_audio":
        if m.get("first_audio_ms"):
            parts.append(f"latency={m['first_audio_ms']}ms")
    elif event == "tts_started":
        if m.get("text_chars"):
            parts.append(f"chars={m['text_chars']}")
        if m.get("streaming") is not None:
            parts.append(f"streaming={m['streaming']}")
    elif event == "tts_playback_done":
        if m.get("sent_frames"):
            parts.append(f"frames={m['sent_frames']}")
        if m.get("completed_normally") is not None:
            parts.append("completed" if m["completed_normally"] else "interrupted")
    elif event in ("local_stop_triggered", "vad_start_suppressed"):
        if m.get("rms") is not None:
            parts.append(f"rms={int(m['rms'])}")
        if m.get("guard_ok") is not None:
            parts.append(f"guard={'ok' if m['guard_ok'] else 'wait'}")
        if m.get("speaking_armed") is not None:
            parts.append(f"armed={m['speaking_armed']}")
    elif event == "barge_in_detected":
        if m.get("latency_ms") is not None:
            parts.append(f"latency={m['latency_ms']}ms")
    elif event == "participant_joined" or event == "subscribed_media":
        if m.get("participant_id"):
            parts.append(f"id={m['participant_id'][:8]}")
    elif event == "bot_waiting_for_participant":
        if m.get("timeout_s"):
            parts.append(f"timeout={m['timeout_s']}s")
    elif event == "tts_mode":
        if m.get("streaming") is not None:
            parts.append(f"streaming={m['streaming']}")
    elif event == "daily_mic_enabled":
        if m.get("audio_processing"):
            parts.append(f"processing={m['audio_processing']}")
    elif event == "remote_audio_first_frame":
        if m.get("len"):
            parts.append(f"len={m['len']}")
        if m.get("rms_as_int16") is not None:
            parts.append(f"rms_i16={m['rms_as_int16']}")
        if m.get("rms_as_float32") is not None:
            parts.append(f"rms_f32={m['rms_as_float32']}")
        if m.get("likely_format"):
            parts.append(f"format={m['likely_format']}")
    elif event == "stderr" or event == "bot_error":
        if m.get("message"):
            msg = m["message"][:60] + "..." if len(m.get("message", "")) > 60 else m.get("message", "")
            parts.append(msg)
        if m.get("error"):
            parts.append(f"error={m['error'][:40]}")
    else:
        # Generic: show first 2-3 simple values
        shown = 0
        for k, v in m.items():
            if shown >= 3:
                break
            if isinstance(v, (str, int, float, bool)) and v is not None:
                if isinstance(v, float):
                    parts.append(f"{k}={v:.1f}")
                elif isinstance(v, str) and len(v) > 20:
                    parts.append(f"{k}={v[:20]}...")
                else:
                    parts.append(f"{k}={v}")
                shown += 1

    return " ".join(parts)


def log_event(event: str, session_id: str = None, utterance_id: str = None, src: str = "worker_local", reason: str = None, metrics: dict | None = None):
    if not LOG_VERBOSE and event not in LOG_MIN_EVENTS:
        return

    if LOG_FORMAT == "json":
        # Machine-readable JSON format
        rec = {
            "ts_ms": _now_ts_ms(),
            "mono_ms": _mono_ms(),
            "event": event,
            "src": src,
        }
        if session_id:
            rec["session_id"] = session_id
        if utterance_id:
            rec["utterance_id"] = utterance_id
        if reason:
            rec["reason"] = reason
        if metrics:
            rec["metrics"] = metrics
        print(json.dumps(rec), flush=True)
    else:
        # Human-readable pretty format
        ts = time.strftime("%H:%M:%S", time.localtime())
        ms = int(time.time() * 1000) % 1000
        icon = _log_icon(event)
        summary = _log_summary(event, reason, metrics)
        # Truncate session_id for display
        sid_short = f" [{session_id[:8]}]" if session_id else ""
        print(f"{ts}.{ms:03d} {icon} {event:<28}{sid_short} {summary}", flush=True)


def log(msg: str, **kwargs):
    """Compat shim for legacy log() calls. Emits as a structured debug event."""
    m = {"message": msg}
    if kwargs:
        m.update(kwargs)
    log_event("debug", metrics=m)


def eprint(*args, **kwargs):
    # Mirror stderr to structured event for consistency
    try:
        msg = " ".join(str(a) for a in args)
        log_event("stderr", reason="python_stderr", metrics={"message": msg})
    except Exception:
        pass
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
    def __init__(self, aggressiveness=2, frame_ms=20, hangover_ms=400, max_utterance_ms=30000):
        self.vad = webrtcvad.Vad(aggressiveness)
        self.frame_ms = frame_ms
        self.hangover_frames = int(hangover_ms / frame_ms)
        self.max_utterance_ms = max_utterance_ms  # Safety timeout for stuck utterances
        self.speaking = False
        self.consec_speech = 0
        self.non_speech = 0
        self.started_at_ms = 0
        self.min_start_frames = 2  # default ~40ms
        self.min_burst_frames = 6  # ~120ms
        # Diagnostic counters
        self._frame_count = 0
        self._speech_while_speaking = 0
        self._nonspeech_while_speaking = 0

    def process_frame(self, pcm16_bytes, sample_rate):
        self._frame_count += 1
        # Calculate RMS for frame
        try:
            arr = np.frombuffer(pcm16_bytes, dtype=np.int16)
            frame_rms = float(np.sqrt(np.mean(arr.astype(np.float64)**2))) if arr.size > 0 else 0.0
        except:
            frame_rms = 0.0
        info = {
            'is_speech': False,
            'consec_speech': self.consec_speech,
            'min_start_frames': self.min_start_frames,
            'speaking': self.speaking,
            'prestart': False,
        }
        # Log first few frames to verify frame size
        if self._frame_count <= 3:
            log_event("vad_frame_info", metrics={"frame": self._frame_count, "bytes": len(pcm16_bytes), "sample_rate": sample_rate, "expected_bytes": int(sample_rate * 0.02) * 2, "rms": int(frame_rms)})
        try:
            is_speech = self.vad.is_speech(pcm16_bytes, sample_rate)
        except Exception as e:
            is_speech = False
            if self._frame_count <= 5:
                log_event("vad_is_speech_error", metrics={"frame": self._frame_count, "error": str(e), "frame_bytes": len(pcm16_bytes), "sample_rate": sample_rate})
        info['is_speech'] = is_speech
        now_ms = int(time.time() * 1000)

        # Track is_speech while in speaking state
        if self.speaking:
            if is_speech:
                self._speech_while_speaking += 1
            else:
                self._nonspeech_while_speaking += 1
            # Log every 50 frames while speaking to diagnose why 'end' never fires
            total_speaking_frames = self._speech_while_speaking + self._nonspeech_while_speaking
            if total_speaking_frames % 50 == 0:
                log_event("vad_speaking_stats", metrics={
                    "speech_frames": self._speech_while_speaking,
                    "nonspeech_frames": self._nonspeech_while_speaking,
                    "speech_pct": int(100 * self._speech_while_speaking / max(1, total_speaking_frames)),
                    "current_non_speech": self.non_speech,
                    "hangover_needed": self.hangover_frames,
                    "frame_rms": int(frame_rms),
                    "is_speech": is_speech,
                })
        if not self.speaking:
            if is_speech:
                self.consec_speech += 1
                info['consec_speech'] = self.consec_speech
                if self.consec_speech >= self.min_start_frames:
                    self.speaking = True
                    self.started_at_ms = now_ms
                    self.non_speech = 0
                    # Reset diagnostic counters on new utterance
                    self._speech_while_speaking = 0
                    self._nonspeech_while_speaking = 0
                    info['speaking'] = True
                    log_event("vad_start_detected", metrics={"frame": self._frame_count})
                    return 'start', now_ms, info
                else:
                    info['prestart'] = True
            else:
                self.consec_speech = 0
                info['consec_speech'] = 0
            return None, None, info
        else:
            # Check for max utterance timeout (safety valve for stuck VAD)
            utterance_dur_ms = now_ms - self.started_at_ms
            if utterance_dur_ms >= self.max_utterance_ms:
                log_event("vad_end_timeout", metrics={
                    "dur_ms": utterance_dur_ms,
                    "max_ms": self.max_utterance_ms,
                    "speech_frames": self._speech_while_speaking,
                    "nonspeech_frames": self._nonspeech_while_speaking,
                })
                self.speaking = False
                self.consec_speech = 0
                self.non_speech = 0
                return 'end', now_ms, info
            if is_speech:
                self.non_speech = 0
                return None, None, info
            else:
                self.non_speech += 1
                if self.non_speech >= self.hangover_frames:
                    dur_frames = int((now_ms - self.started_at_ms) / self.frame_ms)
                    log_event("vad_end_reached_hangover", metrics={
                        "dur_frames": dur_frames,
                        "min_burst": self.min_burst_frames,
                        "speech_frames": self._speech_while_speaking,
                        "nonspeech_frames": self._nonspeech_while_speaking,
                    })
                    self.speaking = False
                    self.consec_speech = 0
                    self.non_speech = 0
                    if dur_frames < self.min_burst_frames:
                        return None, None, info
                    return 'end', now_ms, info
                return None, None, info


class VADManager:
    """Encapsulates VAD gating, counters, guard, energy checks, and WS signaling."""
    def __init__(self, loop, ws_queue, session_id, stop_event, state, vad: VADState):
        self.loop = loop
        self.ws_queue = ws_queue
        self.session_id = session_id or ""
        self.stop_event = stop_event
        self.state = state
        self.vad = vad
        # STT streaming helpers (wired from attach_candidate_vad)
        self.stt_client = None
        self.ring_buffer = None
        self.frame_batcher = None
        self._in_utterance = False
        self._stt_continuous = os.environ.get('STT_CONTINUOUS', 'false').lower() not in ('0','false','no')
        self._stt_suppression_until = 0  # Cooldown timestamp after suppression
        # Ensure per-utterance counters exist in state (reset at utterance start elsewhere)
        self.state.setdefault('vad_counters', {'vad_starts_total': 0, 'vad_stops_allowed': 0, 'vad_suppressed_guard': 0, 'vad_suppressed_energy': 0, 'vad_suppressed_minframes': 0})

    def on_frame(self, frame: bytes):
        ev, ts, vinf = self.vad.process_frame(frame, 48000)
        counters = self.state.setdefault('vad_counters', {
            'vad_starts_total': 0,
            'vad_stops_allowed': 0,
            'vad_suppressed_guard': 0,
            'vad_suppressed_energy': 0,
            'vad_suppressed_minframes': 0,
        })
        now_ms = int(time.time() * 1000)
        # Compute RMS for profiling/feature forwarding
        try:
            arr = np.frombuffer(frame, dtype=np.int16)
            rms_prof = float(np.sqrt(np.mean((arr.astype(np.float64))**2))) if arr.size > 0 else 0.0
        except Exception:
            rms_prof = 0.0
        # Forward VAD feature (RMS) to Orchestrator if connected
        # Use run_coroutine_threadsafe since on_frame is called from audio thread
        try:
            orch = self.state.get('orch_client')
            if orch is not None:
                asyncio.run_coroutine_threadsafe(orch.send_feature(rms_prof), self.loop)
        except Exception:
            pass
        if self.state.get('speaking', False):
            last_sample = self.state.get('rms_last_sample_ts', 0)
            if now_ms - int(last_sample) >= 1000:
                self.state.setdefault('rms_samples', []).append(rms_prof)
                self.state['rms_last_sample_ts'] = now_ms

        if vinf.get('prestart'):
            counters['vad_suppressed_minframes'] += 1

        if ev == 'start':
            counters['vad_starts_total'] += 1
            # Compute gate inputs
            try:
                arr = np.frombuffer(frame, dtype=np.int16)
                rms = float(np.sqrt(np.mean((arr.astype(np.float64))**2))) if arr.size > 0 else 0.0
            except Exception:
                rms = 0.0
            guard_ok = False
            try:
                armed_ts = int(self.state.get('speaking_armed_ts_ms', 0) or 0)
                guard_ms = int(self.state.get('local_stop_guard_ms', 0) or 0)
                guard_ok = (armed_ts > 0) and (now_ms - armed_ts >= guard_ms)
                if guard_ok and not self.state.get('guard_elapsed_logged', False):
                    log_event("local_stop_guard_elapsed", metrics={"guard_ms": guard_ms, "elapsed_ms": now_ms - armed_ts})
                    self.state['guard_elapsed_logged'] = True
            except Exception:
                guard_ok = True
            # Baseline threshold
            min_rms = float(self.state.get('local_stop_min_rms', 0) or 0)
            is_tts_active = bool(self.state.get('speaking', False))
            # Dynamic ambient-relative threshold while TTS is active
            dyn_thresh = min_rms
            if is_tts_active:
                try:
                    rms_vals = self.state.get('rms_samples') or []
                    if rms_vals:
                        vv = sorted(rms_vals)
                        k = max(0, min(len(vv)-1, int(round(0.9*(len(vv)-1)))))
                        p90 = float(vv[k])
                        dyn_thresh = max(min_rms, p90 * 1.5 + 200.0)
                except Exception:
                    dyn_thresh = min_rms
            # Dual-signal agreement: require a recent interim while speaking (optional)
            require_interim = os.environ.get('LOCAL_STOP_REQUIRE_INTERIM', 'true').lower() not in ('0','false','no')
            interim_ok = True
            if require_interim and is_tts_active:
                last_interim_ts = int(self.state.get('stt_last_interim_ts_ms', 0) or 0)
                last_interim_len = int(self.state.get('stt_last_interim_len', 0) or 0)
                interim_win_ms = int(os.environ.get('LOCAL_STOP_INTERIM_WINDOW_MS', '600'))
                min_interim_len = int(os.environ.get('LOCAL_STOP_MIN_INTERIM_LEN', '10'))
                interim_ok = (now_ms - last_interim_ts) <= interim_win_ms and last_interim_len >= min_interim_len
            payload_extra = {"rms": int(rms), "rms_threshold": int(min_rms), "dyn_threshold": int(dyn_thresh), "guard_ok": guard_ok, "speaking_armed": self.state.get('speaking_armed', False), "speaking_armed_ts_ms": self.state.get('speaking_armed_ts_ms', 0), "tts_active": is_tts_active, "interim_ok": interim_ok}
            # Log VAD start with context about whether we're in TTS or listening mode
            log_event("vad_start_fired", session_id=self.session_id, metrics=payload_extra)
            # WS: vad_start
            evt = {"type": "vad_start", "ts_ms": ts, "session_id": self.session_id, "utterance_id": self.state.get('active_utterance_id', ''), "payload": {"source": "candidate_audio", **payload_extra}}
            self.loop.call_soon_threadsafe(self.ws_queue.put_nowait, evt)
            # Gated stop
            self.state['last_vad_ts_ms'] = ts
            # Enterprise: VAD-bounded utterance start (if not in continuous mode)
            # Only start STT utterance if RMS meets minimum threshold (avoid noise triggers)
            stt_min_rms = float(self.state.get('stt_min_rms', 50) or 50)  # Lower than barge-in threshold
            # Check cooldown from previous suppression
            in_cooldown = now_ms < self._stt_suppression_until
            try:
                log_event("debug_vad_start_stt_check", session_id=self.session_id, metrics={
                    "stt_continuous": self._stt_continuous,
                    "stt_client_exists": self.stt_client is not None,
                    "in_utterance": self._in_utterance,
                    "rms": int(rms),
                    "stt_min_rms": int(stt_min_rms),
                    "in_cooldown": in_cooldown,
                })
                if not self._stt_continuous and self.stt_client is not None and not self._in_utterance:
                    # Only start utterance if RMS indicates real speech, not background noise
                    # Strong RMS (2x threshold) can break through cooldown
                    rms_ok = rms >= stt_min_rms and (not in_cooldown or rms >= stt_min_rms * 2)
                    if rms_ok:
                        self._in_utterance = True
                        utt_id = f"utt-{int(time.time()*1000)}"
                        self.state['active_utterance_id'] = utt_id
                        log_event("debug_stt_starting_utterance", session_id=self.session_id, metrics={"utt_id": utt_id, "rms": int(rms)})
                        # Flush ring pre-speech into batcher
                        if self.ring_buffer is not None:
                            flushed = self.ring_buffer.flush_all()
                            if flushed:
                                ds = downsample_48k_to_16k(flushed)
                                if self.frame_batcher is not None:
                                    self.frame_batcher.add(ds)
                        # Use run_coroutine_threadsafe since on_frame is called from audio thread
                        asyncio.run_coroutine_threadsafe(self.stt_client.start_utterance(utt_id), self.loop)
                    else:
                        # Reset VAD state so it can fire a new 'start' when real speech comes
                        self.vad.speaking = False
                        self.vad.consec_speech = 0
                        self.vad.non_speech = 0
                        # Clear pending ring/batcher state to avoid stale audio
                        if self.ring_buffer is not None:
                            self.ring_buffer.flush_all()
                        if self.frame_batcher is not None:
                            self.frame_batcher.flush()
                        # Set cooldown to avoid rapid re-triggers from ambient noise
                        self._stt_suppression_until = now_ms + int(self.state.get('stt_suppression_cooldown_ms', 200) or 200)
                        log_event("stt_start_suppressed", session_id=self.session_id, metrics={"rms": int(rms), "threshold": int(stt_min_rms), "cooldown_ms": 200})
            except Exception as e:
                log_event("debug_stt_start_error", session_id=self.session_id, metrics={"error": str(e)})
            if (self.state.get('local_stop_enabled', True)
                and self.state.get('speaking_armed', False)
                and guard_ok
                and rms >= dyn_thresh
                and interim_ok):
                counters['vad_stops_allowed'] += 1
                log_event("local_stop_triggered", session_id=self.session_id, utterance_id=self.state.get('active_utterance_id', ''), metrics=payload_extra)
                try:
                    self.loop.call_soon_threadsafe(self.stop_event.set)
                except Exception as e1:
                    log_event("local_stop_schedule_error", reason="call_soon_threadsafe", metrics={"error": str(e1)})
                    try:
                        self.stop_event.set()
                    except Exception as e2:
                        log_event("local_stop_schedule_error", reason="fallback_set", metrics={"error": str(e2)})
            else:
                if not guard_ok:
                    counters['vad_suppressed_guard'] += 1
                    log_event("vad_start_suppressed", session_id=self.session_id, utterance_id=self.state.get('active_utterance_id', ''), reason="guard", metrics=payload_extra)
                elif rms < dyn_thresh:
                    counters['vad_suppressed_energy'] += 1
                    log_event("vad_start_suppressed", session_id=self.session_id, utterance_id=self.state.get('active_utterance_id', ''), reason="energy", metrics=payload_extra)
                elif not interim_ok:
                    counters['vad_suppressed_energy'] += 1
                    log_event("vad_start_suppressed", session_id=self.session_id, utterance_id=self.state.get('active_utterance_id', ''), reason="interim", metrics=payload_extra)
        elif ev == 'end':
            evt = {"type": "vad_end", "ts_ms": ts, "session_id": self.session_id, "utterance_id": self.state.get('active_utterance_id', ''), "payload": {"source": "candidate_audio"}}
            self.loop.call_soon_threadsafe(self.ws_queue.put_nowait, evt)
            # Enterprise: VAD-bounded utterance end (if not in continuous mode)
            # Use run_coroutine_threadsafe since on_frame is called from audio thread
            try:
                if not self._stt_continuous and self.stt_client is not None and self._in_utterance:
                    # Flush remaining batched audio
                    if self.frame_batcher is not None:
                        rem = self.frame_batcher.flush()
                        if rem:
                            asyncio.run_coroutine_threadsafe(self.stt_client.send_audio(rem), self.loop)
                    asyncio.run_coroutine_threadsafe(self.stt_client.end_utterance(), self.loop)
                    self._in_utterance = False
            except Exception:
                pass


class TTSMetrics:
    def __init__(self, tts_started_ts_ms: int | None = None):
        self.tts_started_ts_ms = tts_started_ts_ms
        self.tts_request_sent_ts_ms = None
        self.elevenlabs_headers_ts_ms = None
        self.elevenlabs_first_chunk_ts_ms = None
        self.prebuffer_done_ts_ms = None
        self.first_frame_sent_ts_ms = None
        self.producer_first_frame_queued_ts_ms = None
        self.producer_total_chunks = 0
        self.producer_total_bytes = 0
        self.producer_stream_start_ts_ms = None
        self.producer_stream_end_ts_ms = None
        self.queue_peak_frames = 0
        self.queue_sum = 0
        self.queue_samples = 0
        self.underruns = 0
        self.send_start_mono = None

    def mark_request_sent(self):
        self.tts_request_sent_ts_ms = int(time.time() * 1000)

    def mark_headers(self):
        self.elevenlabs_headers_ts_ms = int(time.time() * 1000)

    def mark_first_chunk(self, length: int):
        ts = int(time.time() * 1000)
        self.elevenlabs_first_chunk_ts_ms = ts
        if self.producer_stream_start_ts_ms is None:
            self.producer_stream_start_ts_ms = ts

    def mark_producer_first_frame_queued(self):
        if self.producer_first_frame_queued_ts_ms is None:
            self.producer_first_frame_queued_ts_ms = int(time.time() * 1000)

    def add_chunk(self, length: int):
        self.producer_total_chunks += 1
        self.producer_total_bytes += int(length)

    def mark_stream_end(self):
        self.producer_stream_end_ts_ms = int(time.time() * 1000)

    def mark_prebuffer_done(self):
        self.prebuffer_done_ts_ms = int(time.time() * 1000)

    def mark_first_frame_sent(self):
        self.first_frame_sent_ts_ms = int(time.time() * 1000)

    def begin_send_timing(self):
        self.send_start_mono = time.monotonic()

    def add_queue_sample(self, qsize: int):
        self.queue_peak_frames = max(self.queue_peak_frames, int(qsize))
        self.queue_sum += int(qsize)
        self.queue_samples += 1

    def inc_underrun(self):
        self.underruns += 1

    def emit_breakdown(self, session_id: str | None, utterance_id: str | None):
        breakdown = {
            "tts_started_ts_ms": self.tts_started_ts_ms,
            "tts_request_sent_ts_ms": self.tts_request_sent_ts_ms,
            "elevenlabs_headers_ts_ms": self.elevenlabs_headers_ts_ms,
            "elevenlabs_first_chunk_ts_ms": self.elevenlabs_first_chunk_ts_ms,
            "prebuffer_done_ts_ms": self.prebuffer_done_ts_ms,
            "first_frame_sent_ts_ms": self.first_frame_sent_ts_ms,
        }
        log_event("tts_timing_breakdown", session_id=session_id or "", utterance_id=utterance_id, metrics=breakdown)

    def add_to_payload_and_log(self, payload: dict, session_id: str | None, utterance_id: str | None, sent_frames: int):
        # Drift
        if sent_frames > 0 and self.send_start_mono is not None:
            actual_ms = int((time.monotonic() - self.send_start_mono) * 1000)
            expected_ms = int(sent_frames * 20)
            drift_ms = actual_ms - expected_ms
            payload['drift_ms'] = drift_ms
            payload['expected_ms'] = expected_ms
            payload['actual_ms'] = actual_ms
            warn = abs(drift_ms) > 50
            log_event("tts_playback_drift", session_id=session_id or "", utterance_id=utterance_id, metrics={"expected_ms": expected_ms, "actual_ms": actual_ms, "drift_ms": drift_ms, "warn": warn})
        # Queue
        if self.queue_samples > 0:
            payload['avg_queue_frames'] = float(self.queue_sum) / float(self.queue_samples)
        payload['queue_peak_frames'] = self.queue_peak_frames
        payload['underruns'] = self.underruns
        # Producer
        payload['producer_total_chunks'] = self.producer_total_chunks
        payload['producer_total_bytes'] = self.producer_total_bytes
        ps = self.producer_stream_start_ts_ms
        pe = self.producer_stream_end_ts_ms
        if ps and pe and pe >= ps:
            payload['producer_stream_duration_ms'] = int(pe - ps)


def attach_candidate_vad(transport, ws_queue, session_id, loop, vad, stop_event, state,
                         stt_client=None, ring_buffer=None, frame_batcher=None):
    """Register a candidate-audio VAD callback; bridge to asyncio with call_soon_threadsafe."""
    frame_bytes = int(48000 * 0.02) * 2  # 20ms @48k, 16-bit mono
    buf = bytearray()
    frame_count = [0]  # Use list for nonlocal mutation in nested function
    manager = VADManager(loop, ws_queue, session_id, stop_event, state, vad)
    # Wire STT helpers to VADManager (these were passed in but never assigned)
    manager.stt_client = stt_client
    manager.ring_buffer = ring_buffer
    manager.frame_batcher = frame_batcher

    started_stream = [False]

    # Track RMS after format conversion for diagnostics
    processed_rms_samples = []
    processed_rms_max = [0]

    def handle_frame(pcm_bytes, sample_rate=48000, channels=1):
        nonlocal buf
        frame_count[0] += 1
        # Log every 500 frames (~10 seconds) to confirm we're receiving audio
        if frame_count[0] == 1:
            # Diagnose audio format: check if float32 vs int16
            expected_int16_samples = len(pcm_bytes) // 2
            expected_float32_samples = len(pcm_bytes) // 4
            # Try both interpretations
            try:
                arr_i16 = np.frombuffer(pcm_bytes, dtype=np.int16)
                rms_i16 = float(np.sqrt(np.mean(arr_i16.astype(np.float64)**2))) if arr_i16.size > 0 else 0
            except:
                rms_i16 = -1
            try:
                arr_f32 = np.frombuffer(pcm_bytes, dtype=np.float32)
                # Convert float32 [-1,1] to int16 scale for comparison
                rms_f32_scaled = float(np.sqrt(np.mean((arr_f32 * 32767)**2))) if arr_f32.size > 0 else 0
            except:
                rms_f32_scaled = -1
            log_event("remote_audio_first_frame", metrics={
                "len": len(pcm_bytes), "sr": sample_rate, "ch": channels,
                "rms_as_int16": int(rms_i16), "rms_as_float32": int(rms_f32_scaled),
                "likely_format": "float32" if rms_f32_scaled > rms_i16 * 10 else "int16"
            })
        elif frame_count[0] % 500 == 0:
            log_event("remote_audio_frames_progress", metrics={"frames": frame_count[0]})

        # Detect and convert audio format - Daily may send float32
        bytes_per_sample = len(pcm_bytes) // (sample_rate * channels // 50)  # 20ms frame
        if bytes_per_sample == 4:
            # Float32 format - convert to int16
            pcm_arr = np.frombuffer(pcm_bytes, dtype=np.float32)
            pcm_arr = (pcm_arr * 32767).clip(-32768, 32767).astype(np.int16)
        else:
            # Assume int16
            pcm_arr = np.frombuffer(pcm_bytes, dtype=np.int16)

        # Apply input gain to boost quiet user audio - only when TTS is NOT active
        # to avoid amplifying the bot's own echo during TTS playback
        input_gain = float(os.environ.get('AUDIO_INPUT_GAIN', '1.0'))
        tts_active = state.get('speaking', False)
        if input_gain != 1.0 and pcm_arr.size > 0 and not tts_active:
            pcm_arr = (pcm_arr.astype(np.float32) * input_gain).clip(-32768, 32767).astype(np.int16)
        if channels == 2 and pcm_arr.size % 2 == 0:
            pcm_arr = pcm_arr.reshape(-1, 2).mean(axis=1).astype(np.int16)
        if sample_rate != 48000 and pcm_arr.size > 0:
            pcm_arr = resample_to_48k(pcm_arr, sample_rate)

        # Track RMS after all format conversions for diagnostics
        try:
            if pcm_arr.size > 0:
                rms_processed = float(np.sqrt(np.mean(pcm_arr.astype(np.float64)**2)))
                processed_rms_samples.append(rms_processed)
                if len(processed_rms_samples) > 100:
                    processed_rms_samples.pop(0)
                if rms_processed > processed_rms_max[0]:
                    processed_rms_max[0] = rms_processed
                # Log every 100 frames with processed RMS stats
                if frame_count[0] % 100 == 0:
                    rms_avg = sum(processed_rms_samples) / len(processed_rms_samples) if processed_rms_samples else 0
                    rms_recent_max = max(processed_rms_samples) if processed_rms_samples else 0
                    log_event("audio_processed_rms", metrics={
                        "frame": frame_count[0],
                        "rms_current": int(rms_processed),
                        "rms_avg_100": int(rms_avg),
                        "rms_max_100": int(rms_recent_max),
                        "rms_max_total": int(processed_rms_max[0]),
                        "speaking": state.get('speaking', False)
                    })
        except Exception:
            pass

        b = pcm_arr.tobytes()
        buf.extend(b)
        while len(buf) >= frame_bytes:
            frame = bytes(buf[:frame_bytes])
            del buf[:frame_bytes]
            # Always push frame to ring buffer for STT
            try:
                if ring_buffer is not None:
                    ring_buffer.push(frame)
            except Exception:
                pass
            # Stream frame to STT sidecar
            # Use run_coroutine_threadsafe since handle_frame is called from audio callback thread
            try:
                if stt_client is not None and frame_batcher is not None:
                    if manager._stt_continuous:
                        # Continuous: single long-form utterance
                        if not started_stream[0]:
                            started_stream[0] = True
                            utt = f"utt-{int(time.time()*1000)}"
                            asyncio.run_coroutine_threadsafe(stt_client.start_utterance(utt), loop)
                        # Silence gating when not in VAD utterance to avoid filling provider queue with near-zero frames
                        stt_silence_floor = int(os.environ.get('STT_SILENCE_RMS_FLOOR', '20'))
                        try:
                            arrf = np.frombuffer(frame, dtype=np.int16)
                            frms = float(np.sqrt(np.mean(arrf.astype(np.float64)**2))) if arrf.size > 0 else 0.0
                        except Exception:
                            frms = 0.0
                        if manager._in_utterance or frms >= stt_silence_floor:
                            ds = downsample_48k_to_16k(frame)
                            frame_batcher.add(ds)
                        chunk = frame_batcher.emit_ready()
                        while chunk:
                            asyncio.run_coroutine_threadsafe(stt_client.send_audio(chunk), loop)
                            chunk = frame_batcher.emit_ready()
                    else:
                        # VAD-bounded: only stream during active utterance
                        if manager._in_utterance:
                            ds = downsample_48k_to_16k(frame)
                            frame_batcher.add(ds)
                            chunk = frame_batcher.emit_ready()
                            while chunk:
                                asyncio.run_coroutine_threadsafe(stt_client.send_audio(chunk), loop)
                                chunk = frame_batcher.emit_ready()
            except Exception:
                pass
            manager.on_frame(frame)

    # Try to register callback on transport
    if hasattr(transport, 'on_remote_audio'):
        try:
            transport.on_remote_audio = handle_frame
            log_event("candidate_audio_hook_set", metrics={"method": "on_remote_audio"})
            return
        except Exception as e:
            log_event("candidate_audio_hook_failed", reason="on_remote_audio", metrics={"error": str(e)})
    if hasattr(transport, 'add_remote_audio_callback'):
        try:
            transport.add_remote_audio_callback(handle_frame)
            log_event("candidate_audio_hook_set", metrics={"method": "add_remote_audio_callback"})
            return
        except Exception as e:
            log_event("candidate_audio_hook_failed", reason="add_remote_audio_callback", metrics={"error": str(e)})
    log_event("candidate_audio_rx_unavailable")


async def playback_task(transport, pcm16_bytes, sr, stop_event, loop, ws_queue, session_id, utterance_id, state):
    """Send audio in 20ms frames with precise pacing, drift metrics, and early-wake stop."""
    bytes_per_sample = 2
    samples_per_frame = int(sr * 0.02)
    bytes_per_frame = samples_per_frame * bytes_per_sample
    pos = 0
    sent_frames = 0
    started_ms = int(time.time() * 1000)
    method = 'unknown'
    # Precise pacing using monotonic clock
    frame_duration = 0.02
    next_frame_time = time.monotonic()
    tm = TTSMetrics(state.get('tts_started_ts_ms'))
    tm.begin_send_timing()

    while pos < len(pcm16_bytes):
        if stop_event.is_set():
            break
        now = time.monotonic()
        sleep_time = next_frame_time - now
        if sleep_time > 0:
            try:
                # Only sleep if meaningful
                if sleep_time > 0.005:
                    await asyncio.wait_for(stop_event.wait(), timeout=sleep_time)
            except asyncio.TimeoutError:
                pass
        # If we're behind, catch up without extra sleep
        next_frame_time += frame_duration
        chunk = pcm16_bytes[pos:pos + bytes_per_frame]
        pos += bytes_per_frame
        if not chunk:
            break
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
                # Arm local-stop after first frame and record ts
                state['speaking_armed'] = True
                state['speaking_armed_ts_ms'] = int(time.time() * 1000)
                log_event("tts_first_frame_sent", session_id=session_id or "", utterance_id=utterance_id, metrics={"len": len(chunk)})
                tm.mark_first_frame_sent()
        except Exception as e:
            eprint("publish error:", e)
            raise

    duration_ms = int(time.time() * 1000) - started_ms
    log_event("audio_publish_summary", metrics={
        "sr": sr,
        "channels": 1,
        "width_bytes": 2,
        "frame_ms": 20,
        "samples_per_frame": samples_per_frame,
        "sent_frames": sent_frames,
        "duration_ms": duration_ms,
        "method": method,
    })

    # WS: tts_stopped (reason determined by stop_event)
    reason = "interrupted" if stop_event.is_set() else "completed"
    if session_id and not state.get('tts_stop_emitted', False):
        now_ts = int(time.time() * 1000)
        payload = {"source": "worker_local", "reason": reason}
        vad_ts = state.get('last_vad_ts_ms')
        if reason == 'interrupted' and isinstance(vad_ts, (int, float)) and vad_ts > 0:
            payload['barge_in_ms'] = max(0, now_ts - int(vad_ts))
        # Add drift/queue metrics for parity with streaming path (queue stats will be minimal here)
        tm.add_to_payload_and_log(payload, session_id, utterance_id, sent_frames)
        evt = {"type": "tts_stopped", "ts_ms": now_ts, "session_id": session_id, "utterance_id": utterance_id, "payload": payload}
        state['tts_stop_emitted'] = True
        await ws_queue.put(evt)
        try:
            orch = state.get('orch_client')
            if orch is not None:
                await orch.send_tts_event('stopped', reason=reason)
        except Exception:
            pass
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


def _producer_stream_elevenlabs(eleven_api_key, voice_id, text, loop, queue, stop_flag: threading.Event, metrics):
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
    # Mark request start for timing breakdown
    metrics.mark_request_sent()
    log_event("tts_producer_http_request_start")
    chunk_count = 0
    try:
        with requests.post(url, headers=headers, data=json.dumps(data), stream=True, timeout=30) as resp:
            log_event("tts_producer_http_response", metrics={"status": resp.status_code})
            metrics.mark_headers()
            resp.raise_for_status()
            for chunk in resp.iter_content(chunk_size=4096):
                chunk_count += 1
                if chunk_count == 1:
                    log_event("tts_producer_first_chunk", metrics={"len": len(chunk)})
                    metrics.mark_first_chunk(len(chunk))
                if stop_flag.is_set():
                    log_event("tts_producer_stop_flag", metrics={"chunk_count": chunk_count})
                    break
                if not chunk:
                    continue
                # Buffer raw bytes to ensure 2-byte alignment for int16
                raw_buf.extend(chunk)
                # Update producer metrics
                metrics.add_chunk(len(chunk))
                aligned_len = (len(raw_buf) // 2) * 2
                if aligned_len == 0:
                    continue
                aligned_bytes = bytes(raw_buf[:aligned_len])
                del raw_buf[:aligned_len]
                # Native 48kHz PCM16 - no resampling needed
                out_buf.extend(aligned_bytes)
                # Emit complete 20ms frames with backpressure
                for frm in slice_frames(out_buf, frame_bytes_48k):
                    if metrics.producer_first_frame_queued_ts_ms is None:
                        metrics.mark_producer_first_frame_queued()
                        log_event("tts_producer_first_frame_queued")
                    fut = asyncio.run_coroutine_threadsafe(queue.put(frm), loop)
                    try:
                        fut.result()  # backpressure
                    except Exception as e:
                        log_event("tts_queue_put_failed", metrics={"error": str(e)})
                        return
            log_event("tts_producer_http_stream_finished")
            metrics.mark_stream_end()
            # Send sentinel to signal completion
            fut = asyncio.run_coroutine_threadsafe(queue.put(None), loop)
            try:
                fut.result()
            except Exception:
                pass
    except Exception as e:
        log_event("tts_producer_exception", metrics={"error": str(e)})
        # Send sentinel even on error
        try:
            fut = asyncio.run_coroutine_threadsafe(queue.put(None), loop)
            fut.result()
        except Exception:
            pass


async def tts_streaming_play(loop, transport, eleven_api_key, voice_id, text, stop_event, ws_queue, session_id, utterance_id, state):
    """Streaming TTS end-to-end: producer + consumer with prebuffer and underrun handling."""
    log_event("tts_streaming_play_started", session_id=session_id or "", utterance_id=utterance_id)
    queue = asyncio.Queue(maxsize=25)  # ~500ms at 20ms frames
    frame_bytes = int(48000 * 0.02) * 2
    tm = TTSMetrics(state.get('tts_started_ts_ms'))
    stop_flag = threading.Event()

    def start_producer():
        log_event("tts_producer_start", session_id=session_id or "", utterance_id=utterance_id)
        _producer_stream_elevenlabs(eleven_api_key, voice_id, text, loop, queue, stop_flag, tm)
        log_event("tts_producer_finished", session_id=session_id or "", utterance_id=utterance_id)

    # Start producer in threadpool
    log_event("tts_producer_launch", session_id=session_id or "", utterance_id=utterance_id)
    prod_fut = loop.run_in_executor(None, start_producer)

    # Prebuffer 10-25 frames (200-500ms) to smooth network jitter
    prebuffer_target = int(os.environ.get('TTS_PREBUFFER_FRAMES', str(state.get('tts_prebuffer_frames_next', 15))))
    prebuffer_target = max(10, min(25, prebuffer_target))
    prebuffer_timeout_secs = int(os.environ.get('TTS_PREBUFFER_TIMEOUT_SECS', '30'))
    log_event("tts_prebuffer_wait", session_id=session_id or "", utterance_id=utterance_id, metrics={"target_frames": prebuffer_target, "timeout_s": prebuffer_timeout_secs})
    prebuffer_timeout = time.time() + prebuffer_timeout_secs
    while queue.qsize() < prebuffer_target and not stop_event.is_set():
        if time.time() > prebuffer_timeout:
            log_event("tts_prebuffer_timeout", session_id=session_id or "", utterance_id=utterance_id)
            break
        # Check if producer finished early (peek for None sentinel)
        if not prod_fut.done():
            await asyncio.sleep(0.01)
        else:
            log_event("tts_producer_finished_during_prebuffer", session_id=session_id or "", utterance_id=utterance_id)
            break
    tm.add_queue_sample(queue.qsize())
    log_event("tts_prebuffer_done", session_id=session_id or "", utterance_id=utterance_id, metrics={"queue_size": queue.qsize()})
    tm.mark_prebuffer_done()

    # Consumer loop with monotonic timing for consistent frame rate
    sent_frames = 0
    first_audio_emitted = False
    completed_normally = False
    frame_duration = 0.02  # 20ms per frame
    next_frame_time = time.monotonic()
    send_start_mono = None

    try:
        while not stop_event.is_set():
            try:
                frm = await asyncio.wait_for(queue.get(), timeout=0.5)
            except asyncio.TimeoutError:
                # True underrun - no data for 500ms during streaming
                log_event("tts_consumer_underrun", session_id=session_id or "", utterance_id=utterance_id)
                tm.inc_underrun()
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
                    try:
                        orch = state.get('orch_client')
                        if orch is not None:
                            await orch.send_tts_event('stopped', reason=reason)
                    except Exception:
                        pass
                break
            # Check for sentinel (None) indicating producer is done
            if frm is None:
                log_event("tts_stream_complete", session_id=session_id or "", utterance_id=utterance_id)
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
                    log_event("tts_first_frame_sent", session_id=session_id or "", utterance_id=utterance_id, metrics={"len": len(frm)})
                    state['speaking_armed'] = True
                    state['speaking_armed_ts_ms'] = int(time.time() * 1000)
                    log_event("speaking_armed", session_id=session_id or "", utterance_id=utterance_id, metrics={"speaking_armed_ts_ms": state['speaking_armed_ts_ms']})
                    tm.mark_first_frame_sent()
                    tm.emit_breakdown(session_id, utterance_id)
                    tm.begin_send_timing()
                if not first_audio_emitted:
                    first_audio_emitted = True
                    if session_id:
                        now_ts = int(time.time() * 1000)
                        tts_started_ts = state.get('tts_started_ts_ms', now_ts)
                        first_audio_ms = max(0, now_ts - int(tts_started_ts))
                        evt = {"type": "tts_first_audio", "ts_ms": now_ts, "session_id": session_id, "utterance_id": utterance_id, "payload": {"first_audio_ms": first_audio_ms}}
                        await ws_queue.put(evt)
                        log_event("tts_first_audio", session_id=session_id or "", utterance_id=utterance_id, metrics={"first_audio_ms": first_audio_ms})
                        try:
                            oc = state.get('orch_client')
                            if oc is not None:
                                await oc.send_tts_event('first_audio', first_audio_ms=first_audio_ms)
                        except Exception:
                            pass
            except Exception as e:
                log_event("tts_transport_send_error", session_id=session_id or "", utterance_id=utterance_id, metrics={"error": str(e)})
                stop_event.set()
                break

            qsz = queue.qsize()
            tm.add_queue_sample(qsz)
            next_frame_time += frame_duration  # Schedule next frame exactly 20ms later
        # Emit tts_stopped with appropriate reason and include VAD/RMS profiling
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
                log_event("barge_in_detected", session_id=session_id, utterance_id=utterance_id, metrics={"latency_ms": barge_in_ms, "path": "VAD->stop"})
            # Attach VAD suppression counters
            if isinstance(state.get('vad_counters'), dict):
                payload['vad_counters'] = state['vad_counters']
            # Attach speaking armed ts
            if state.get('speaking_armed_ts_ms'):
                payload['speaking_armed_ts_ms'] = int(state['speaking_armed_ts_ms'])
            # RMS profiling percentiles
            def _pct(values, p):
                if not values:
                    return 0.0
                vv = sorted(values)
                k = max(0, min(len(vv)-1, int(round((p/100.0)*(len(vv)-1)))))
                return float(vv[k])
            rms_vals = state.get('rms_samples') or []
            if rms_vals:
                payload['rms_p50'] = _pct(rms_vals, 50)
                payload['rms_p90'] = _pct(rms_vals, 90)
            # Add drift, queue, and producer metrics
            tm.add_to_payload_and_log(payload, session_id, utterance_id, sent_frames)

            evt = {"type": "tts_stopped", "ts_ms": now_ts, "session_id": session_id, "utterance_id": utterance_id, "payload": payload}
            state['tts_stop_emitted'] = True
            await ws_queue.put(evt)
            # Notify orchestrator that TTS stopped
            try:
                orch = state.get('orch_client')
                if orch is not None:
                    await orch.send_tts_event('stopped', reason=reason)
            except Exception:
                pass
        log_event("tts_playback_done", session_id=session_id or "", utterance_id=utterance_id, metrics={"sent_frames": sent_frames, "completed_normally": completed_normally})
    finally:
        # Adapt prebuffer for next utterance based on underruns
        try:
            nxt = prebuffer_target
            if tm.underruns > 0:
                nxt = min(25, prebuffer_target + 2)
            else:
                nxt = max(10, prebuffer_target - 1)
            state['tts_prebuffer_frames_next'] = nxt
            log_event("tts_prebuffer_adapt", session_id=session_id or "", utterance_id=utterance_id, metrics={"this": prebuffer_target, "next": nxt, "underruns": tm.underruns})
        except Exception:
            pass
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
        if session_id:
            now_ts = int(time.time() * 1000)
            evt = {"type": "tts_queue_peak_frames", "ts_ms": now_ts, "session_id": session_id, "utterance_id": utterance_id, "payload": {"peak_frames": tm.queue_peak_frames}}
            with contextlib.suppress(Exception):
                await ws_queue.put(evt)
        # Disarm local-stop after playback concludes and clear stop flag for next turns
        state['speaking_armed'] = False
        try:
            stop_event.clear()
        except Exception:
            pass


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
        self._use_participant_audio = False  # When True, use per-participant audio instead of speaker

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
            log_event("daily_mic_enabled", metrics={"audio_processing": "disabled"})
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
            log_event("daily_mic_enabled", metrics={"audio_processing": "default"})

        # Start a background reader to pull frames from the virtual speaker
        # and forward them into the VAD path via _on_speaker_audio.
        def _speaker_reader():
            try:
                import numpy as np
                sr = getattr(self.speaker, 'sample_rate', 48000) or 48000
                ch = getattr(self.speaker, 'channels', 1) or 1
                num_frames = int(sr / 100) * 2  # 20ms
                self._speaker_read_errors = 0
                self._speaker_frame_count = 0
                self._speaker_nonzero_count = 0
                # RMS tracking for diagnostics
                self._rms_samples = []
                self._rms_max = 0
                log_event("speaker_reader_started", metrics={"sr": sr, "ch": ch, "frames_per_read": num_frames})
                while True:
                    try:
                        data = self.speaker.read_frames(num_frames)
                        # Skip speaker audio if using per-participant audio (to avoid duplicate/loopback audio)
                        if self._use_participant_audio:
                            time.sleep(0.02)  # Still need to drain the buffer at ~20ms intervals
                            continue
                        if data and self._remote_audio_callback:
                            self._speaker_frame_count += 1
                            # Check if data has any non-zero content
                            if isinstance(data, bytes) and any(b != 0 for b in data[:100]):
                                self._speaker_nonzero_count += 1

                            # Calculate RMS at speaker reader level (before any processing)
                            rms_raw = 0
                            try:
                                if isinstance(data, bytes):
                                    # Try int16 interpretation
                                    arr = np.frombuffer(data, dtype=np.int16)
                                    if arr.size > 0:
                                        rms_raw = float(np.sqrt(np.mean(arr.astype(np.float64)**2)))
                                        self._rms_samples.append(rms_raw)
                                        if len(self._rms_samples) > 100:
                                            self._rms_samples.pop(0)
                                        if rms_raw > self._rms_max:
                                            self._rms_max = rms_raw
                            except Exception:
                                pass

                            # Log every 500 frames (~10s) with RMS stats
                            if self._speaker_frame_count == 1 or self._speaker_frame_count % 500 == 0:
                                rms_avg = sum(self._rms_samples) / len(self._rms_samples) if self._rms_samples else 0
                                rms_recent_max = max(self._rms_samples) if self._rms_samples else 0
                                log_event("speaker_reader_stats", metrics={
                                    "frames": self._speaker_frame_count,
                                    "nonzero_frames": self._speaker_nonzero_count,
                                    "data_len": len(data) if data else 0,
                                    "rms_current": int(rms_raw),
                                    "rms_avg_100": int(rms_avg),
                                    "rms_max_100": int(rms_recent_max),
                                    "rms_max_total": int(self._rms_max)
                                })
                            # Log high RMS events (potential speech)
                            elif rms_raw > 50:
                                log_event("speaker_high_rms", metrics={
                                    "frame": self._speaker_frame_count,
                                    "rms": int(rms_raw)
                                })

                            # Bridge into existing callback path
                            self._remote_audio_callback(data, sample_rate=sr, channels=ch)
                        else:
                            # Back off slightly if no data
                            time.sleep(0.005)
                    except Exception as e:
                        self._speaker_read_errors += 1
                        log_event("speaker_read_error", reason="read_frames", metrics={"error": str(e), "errors_total": self._speaker_read_errors})
                        time.sleep(0.05)
            except Exception as e:
                log_event("speaker_reader_init_error", metrics={"error": str(e)})

        t = threading.Thread(target=_speaker_reader, daemon=True)
        t.start()

    def _on_joined(self, data, error):
        if error:
            log_event("daily_join_error", metrics={"error": str(error)})
            return
        self._joined = True
        log_event("daily_joined")

    def on_participant_joined(self, participant):
        """Daily SDK event handler - called when any participant joins."""
        # Ignore the bot itself (local participant)
        if participant.get("info", {}).get("isLocal", False):
            return
        participant_id = participant.get("id", "unknown")
        # Log full participant info for debugging
        info = participant.get("info", {})
        media = participant.get("media", {})
        log_event("participant_joined", metrics={
            "participant_id": participant_id,
            "user_name": info.get("userName", ""),
            "is_owner": info.get("isOwner", False),
            "audio_state": media.get("microphone", {}).get("state", "unknown"),
            "camera_state": media.get("camera", {}).get("state", "unknown")
        })
        self._user_participant_id = participant_id
        # Subscribe to this participant's media
        try:
            self.client.update_subscriptions(participant_settings={
                participant_id: {"media": "subscribed"}
            })
            # Check subscription result
            try:
                updated_participants = self.client.participants()
                p_data = updated_participants.get(participant_id, {})
                p_media = p_data.get("media", {})
                mic_sub = p_media.get("microphone", {}).get("subscribed", "unknown")
                mic_state = p_media.get("microphone", {}).get("state", "unknown")
                log_event("subscribed_media", metrics={
                    "participant_id": participant_id,
                    "mic_subscribed": mic_sub,
                    "mic_state": mic_state
                })
            except Exception:
                log_event("subscribed_media", metrics={"participant_id": participant_id})
        except Exception as e:
            eprint(f"subscribe failed for {participant_id}: {e}")
            log_event("subscribe_failed", metrics={"participant_id": participant_id, "error": str(e)})

        # Set up per-participant audio renderer to receive ONLY this participant's audio
        # (not the bot's own audio loopback from the speaker)
        try:
            import numpy as np
            self._participant_audio_frames = 0
            self._participant_audio_rms_max = 0

            def on_participant_audio(participant_id, audio_data, client):
                """Called when audio is received from the remote participant.

                Args:
                    participant_id: ID of the participant (string)
                    audio_data: AudioData with audio_frames, sample_rate, num_channels
                    client: The Daily client object
                """
                if self._remote_audio_callback and audio_data:
                    try:
                        self._participant_audio_frames += 1
                        frames = audio_data.audio_frames

                        # Calculate RMS for logging
                        rms = 0
                        if frames and isinstance(frames, bytes):
                            try:
                                arr = np.frombuffer(frames, dtype=np.int16)
                                if arr.size > 0:
                                    rms = int(np.sqrt(np.mean(arr.astype(np.float64)**2)))
                                    if rms > self._participant_audio_rms_max:
                                        self._participant_audio_rms_max = rms
                            except Exception:
                                pass

                        # Log first frame and periodically
                        if self._participant_audio_frames == 1:
                            log_event("participant_audio_first_frame", metrics={
                                "participant_id": participant_id,
                                "sample_rate": audio_data.sample_rate,
                                "channels": audio_data.num_channels,
                                "frame_len": len(frames) if frames else 0,
                                "rms": rms
                            })
                        elif self._participant_audio_frames % 500 == 0:
                            log_event("participant_audio_stats", metrics={
                                "frames": self._participant_audio_frames,
                                "rms": rms,
                                "rms_max": self._participant_audio_rms_max
                            })
                        elif rms > 500:  # Log high RMS events (potential speech)
                            log_event("participant_audio_speech", metrics={
                                "frame": self._participant_audio_frames,
                                "rms": rms
                            })

                        # audio_data is daily.AudioData with audio_frames, sample_rate, num_channels
                        self._remote_audio_callback(
                            frames,
                            sample_rate=audio_data.sample_rate,
                            channels=audio_data.num_channels
                        )
                    except Exception as e:
                        eprint(f"participant audio callback error: {e}")

            self.client.set_audio_renderer(participant_id, on_participant_audio)
            self._use_participant_audio = True  # Switch to per-participant audio
            log_event("audio_renderer_set", metrics={"participant_id": participant_id})
        except Exception as e:
            eprint(f"set_audio_renderer failed for {participant_id}: {e}")
            log_event("audio_renderer_failed", metrics={"participant_id": participant_id, "error": str(e)})

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
                    log_event("participant_already_present", metrics={"participant_id": pid})
                    self._user_participant_id = pid
                    # Ensure subscription for already-present participant
                    try:
                        self.client.update_subscriptions(participant_settings={
                            pid: {"media": "subscribed"}
                        })
                        log_event("subscribed_media", metrics={"participant_id": pid})
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

    log_event("bot_joining")

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
    state['last_activity_ms'] = int(time.time() * 1000)
    state['local_stop_enabled'] = os.environ.get('LOCAL_STOP_ENABLED', 'true').lower() not in ('0', 'false', 'no')
    # Guard to avoid barge-in before users hear anything; default 500ms (tunable)
    try:
        state['local_stop_guard_ms'] = int(os.environ.get('LOCAL_STOP_GUARD_MS', '1200'))
    except Exception:
        state['local_stop_guard_ms'] = 1200
    # Minimum RMS (0-32767) to accept VAD start as real speech for barge-in; default 1200 (tunable)
    try:
        state['local_stop_min_rms'] = int(os.environ.get('LOCAL_STOP_MIN_RMS', '1200'))
    except Exception:
        state['local_stop_min_rms'] = 1200
    # Minimum RMS for STT utterance creation (lower than barge-in to capture quiet speech); default 50
    try:
        state['stt_min_rms'] = int(os.environ.get('STT_MIN_RMS', '50'))
    except Exception:
        state['stt_min_rms'] = 50
    # Cooldown after STT suppression to avoid rapid re-triggers from ambient noise; default 200ms
    try:
        state['stt_suppression_cooldown_ms'] = int(os.environ.get('STT_SUPPRESSION_COOLDOWN_MS', '200'))
    except Exception:
        state['stt_suppression_cooldown_ms'] = 200

    ws_task = None
    if ws_url:
        ws_task = asyncio.create_task(run_ws(ws_url, worker_token, session_id, ws_queue, stop_event, state))

    # Join Daily synchronously (library is sync); run in thread to avoid blocking loop
    loop = asyncio.get_running_loop()
    try:
        transport = await loop.run_in_executor(None, try_join_daily, room_url, token)
        log_event("bot_joined", session_id=session_id or "")
    except Exception as e:
        eprint("join failed:", e)
        log_event("bot_exit")
        return

    # Wait for user to join before speaking
    participant_timeout = int(os.environ.get("BOT_PARTICIPANT_TIMEOUT_SECONDS", "120"))
    log_event("bot_waiting_for_participant", session_id=session_id or "", metrics={"timeout_s": participant_timeout})
    participant_found = await loop.run_in_executor(None, transport.wait_for_participant, participant_timeout)
    if not participant_found:
        log_event("bot_participant_timeout", session_id=session_id or "")
        log_event("bot_exit", session_id=session_id or "")
        return
    log_event("bot_participant_ready", session_id=session_id or "")

    # Debug: log key environment variables
    log_event("debug_env", session_id=session_id or "", metrics={
        "ORCH_ADDR": os.environ.get('ORCH_ADDR', '<not set>'),
        "STT_ENABLED": os.environ.get('STT_ENABLED', '<not set>'),
        "STT_UDS_PATH": os.environ.get('STT_UDS_PATH', '<not set>'),
    })

    # Orchestrator control: connect by default to local orchestrator unless explicitly disabled.
    orch = None
    try:
        orch_addr = os.environ.get('ORCH_ADDR') or 'localhost:9090'
        log_event("debug_orch_connecting", session_id=session_id or "", metrics={"addr": orch_addr})
        if not session_id:
            session_id = f"sess-{int(time.time()*1000)}"
        orch = GatewayControlClient(session_id, loop, log_event, stop_event, state)
        await orch.connect()
        await orch.send_session_open(room_url)
        state['orch_client'] = orch
        log_event("orchestrator_connected", session_id=session_id or "", metrics={"addr": orch_addr})
        # Wire StartTTS to TTS service client
        tts_client = TTSClient(loop, log_event)
        voice_id_env = os.environ.get('ELEVENLABS_VOICE_ID', '')

        # Debounced, streaming TTS for LLM sentences: accumulate then stream for smoother replies
        state['tts_accum_buf'] = []
        state['tts_accum_task'] = None
        try:
            debounce_ms = int(os.environ.get('TTS_LLM_ACCUM_DEBOUNCE_MS', '200'))
        except Exception:
            debounce_ms = 200

        async def _flush_tts_accum():
            buf = state.get('tts_accum_buf') or []
            state['tts_accum_buf'] = []
            if not buf:
                return
            phrase_text = " ".join(buf).strip()
            if not phrase_text:
                return
            # New utterance id per flush
            utterance_id2 = f"u-{int(time.time()*1000)}"
            state['active_utterance_id'] = utterance_id2
            state['tts_started_ts_ms'] = int(time.time() * 1000)
            state['tts_stop_emitted'] = False
            state['speaking'] = True
            log_event("orchestrator_start_tts_received", session_id=session_id or "", metrics={"text_len": len(phrase_text)})
            log_event("tts_started", session_id=session_id or "", utterance_id=utterance_id2, metrics={"text_chars": len(phrase_text), "streaming": True})
            log_event("tts_playback_start", session_id=session_id or "", utterance_id=utterance_id2)
            try:
                oc = state.get('orch_client')
                if oc is not None:
                    await oc.send_tts_event('started')
            except Exception:
                pass
            try:
                # Use streaming playback for smoother pacing
                await tts_streaming_play(loop, transport, eleven_api_key, voice_id_env, phrase_text, stop_event, ws_queue, session_id, utterance_id2, state)
            except Exception:
                log_event("tts_streaming_play_error", session_id=session_id or "", utterance_id=utterance_id2)
            finally:
                state['speaking'] = False
                state['active_utterance_id'] = ''

        async def _on_start_tts(text: str):
            # Accumulate short sentences briefly to avoid staccato speech
            state.setdefault('tts_accum_buf', []).append(text)
            # Mark activity on LLM sentence
            state['last_activity_ms'] = int(time.time() * 1000)
            t = state.get('tts_accum_task')
            if t and not t.done():
                try:
                    t.cancel()
                except Exception:
                    pass
            async def _delayed_flush():
                try:
                    await asyncio.sleep(debounce_ms / 1000.0)
                    await _flush_tts_accum()
                except asyncio.CancelledError:
                    return
            state['tts_accum_task'] = asyncio.create_task(_delayed_flush())

        orch.on_start_tts = _on_start_tts
    except Exception as e:
        log_event("orchestrator_connect_error", session_id=session_id or "", metrics={"error": str(e)})

    # Candidate audio VAD wiring + STT sidecar streaming
    vad_agg = int(os.environ.get('WORKER_VAD_AGGRESSIVENESS', '2'))
    vad_hangover = int(os.environ.get('WORKER_VAD_HANGOVER_MS', '400'))
    vad_max_utt = int(os.environ.get('WORKER_VAD_MAX_UTTERANCE_MS', '30000'))
    vad = VADState(aggressiveness=vad_agg, frame_ms=20, hangover_ms=vad_hangover, max_utterance_ms=vad_max_utt)
    log_event("vad_config", session_id=session_id or "", metrics={"aggressiveness": vad_agg, "hangover_ms": vad_hangover, "hangover_frames": vad.hangover_frames, "max_utterance_ms": vad_max_utt})
    # Enable STT by default for E2E; allow disabling via STT_ENABLED=false
    stt_enabled = os.environ.get('STT_ENABLED', 'true').lower() not in ('0', 'false', 'no')
    log_event("debug_stt_config", session_id=session_id or "", metrics={
        "stt_enabled": stt_enabled,
        "stt_uds_path": os.environ.get('STT_UDS_PATH', '/run/app/stt.sock'),
    })
    stt_client = None
    ring = RingBuffer(capacity_ms=int(os.environ.get('RING_BUFFER_MS','300')), hard_cap_ms=int(os.environ.get('RING_BUFFER_HARD_CAP_MS','500')))
    batcher = FrameBatcher(batch_ms=int(os.environ.get('STT_BATCH_MS','100')))
    if stt_enabled:
        try:
            log_event("debug_stt_creating_client", session_id=session_id or "")
            stt_client = STTSidecarClient(session_id, loop, log_event, ws_queue, state)
            if orch:
                stt_client.attach_orchestrator(orch)
                log_event("debug_stt_attached_orch", session_id=session_id or "")
            log_event("debug_stt_preconnecting", session_id=session_id or "")
            await stt_client.preconnect()
            log_event("debug_stt_preconnected", session_id=session_id or "")
        except Exception as e:
            log_event("stt_error", session_id=session_id or "", metrics={"error": str(e)})
            stt_client = None
    else:
        log_event("debug_stt_disabled", session_id=session_id or "")
    try:
        attach_candidate_vad(transport, ws_queue, session_id, loop, vad, stop_event, state,
                             stt_client=stt_client, ring_buffer=ring, frame_batcher=batcher)
    except Exception as e:
        eprint("candidate audio hook unavailable:", e)

    # Decide streaming vs non-streaming
    use_streaming = os.environ.get('ELEVENLABS_STREAMING', 'true').lower() not in ('0', 'false', 'no')
    log_event("tts_mode", session_id=session_id or "", metrics={"streaming": use_streaming})

    # Emit tts_started and mark speaking
    log_event("tts_setting_speaking_state", session_id=session_id or "")
    speaking = True
    state['speaking'] = True
    state['speaking_armed'] = False  # only arm after first audio is sent
    # Require multiple consecutive speech frames while TTS is active to reduce false positives
    try:
        vad_start_frames_during_tts = int(os.environ.get('WORKER_VAD_MIN_START_FRAMES_WHILE_TTS', '10'))
    except Exception:
        vad_start_frames_during_tts = 10
    vad.min_start_frames = max(1, vad_start_frames_during_tts)
    # Reset per-utterance counters and RMS profiling
    state['vad_counters'] = {'vad_starts_total': 0, 'vad_stops_allowed': 0, 'vad_suppressed_guard': 0, 'vad_suppressed_energy': 0, 'vad_suppressed_minframes': 0}
    state['rms_samples'] = []
    state['rms_last_sample_ts'] = 0
    state['guard_elapsed_logged'] = False
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
    log_event("tts_started", session_id=session_id or "", utterance_id=utterance_id, metrics={"text_chars": len(phrase), "streaming": use_streaming})

    log_event("tts_playback_start", session_id=session_id or "", utterance_id=utterance_id)
    # Notify Orchestrator that playback has started
    try:
        oc = state.get('orch_client')
        if oc is not None:
            await oc.send_tts_event('started')
    except Exception:
        pass
    try:
        if use_streaming:
            log_event("tts_streaming_mode", session_id=session_id or "", utterance_id=utterance_id)
            await tts_streaming_play(loop, transport, eleven_api_key, voice_id, phrase, stop_event, ws_queue, session_id, utterance_id, state)
        else:
            # Fallback: fetch-then-play
            log_event("tts_fetch_start", session_id=session_id or "", utterance_id=utterance_id)
            wav_bytes = await loop.run_in_executor(None, fetch_tts_wav, eleven_api_key, voice_id, phrase)
            pcm_arr, sr_in, _ = decode_wav_pcm16(wav_bytes)
            pcm_arr_48k = resample_to_48k(pcm_arr, sr_in)
            pcm16_bytes = pcm_arr_48k.tobytes()
            log_event("tts_fetch_done", session_id=session_id or "", utterance_id=utterance_id, metrics={"bytes": len(pcm16_bytes)})
            await playback_task(transport, pcm16_bytes, 48000, stop_event, loop, ws_queue, session_id, utterance_id, state)
    except Exception:
        log_event("bot_error", session_id=session_id or "", reason="publish_send_failed")
        log_event("bot_exit", session_id=session_id or "")
        return
    finally:
        speaking = False
        state['speaking'] = False
        state['active_utterance_id'] = ''
        vad.min_start_frames = 2  # restore default when not speaking
        state['tts_stop_emitted'] = False

    # Stay alive while conversation is active or until idle timeout expires
    # BOT_STAY_CONNECTED_SECONDS remains a hard cap if set high; BOT_IDLE_EXIT_SECONDS controls idle-based exit
    idle_exit_s = int(os.environ.get('BOT_IDLE_EXIT_SECONDS', '60'))
    idle_log_next = 0
    log_event("bot_sleep_start", session_id=session_id, metrics={"idle_exit_s": idle_exit_s})
    start_ts = time.time()
    while True:
        now = time.time()
        # Never exit while actively speaking or with an active utterance
        if state.get('speaking') or state.get('active_utterance_id'):
            state['last_activity_ms'] = int(now * 1000)
        # Compute idle seconds since last activity
        last_ms = int(state.get('last_activity_ms', int(now * 1000)))
        idle_for = max(0, int(now - (last_ms / 1000)))
        # Optional hard cap: respect BOT_STAY_CONNECTED_SECONDS if configured
        if stay_s > 0 and (now - start_ts) >= stay_s and idle_for >= idle_exit_s and not state.get('speaking'):
            break
        # Idle-based exit
        if idle_for >= idle_exit_s and not state.get('speaking') and not state.get('active_utterance_id'):
            break
        # Periodic idle updates
        if int(now) >= idle_log_next:
            log_event("bot_idle_status", session_id=session_id, metrics={"idle_for_s": idle_for, "idle_exit_s": idle_exit_s, "speaking": state.get('speaking', False)})
            idle_log_next = int(now) + 10
        # Wait 1s or until barge-in
        try:
            await asyncio.wait_for(stop_event.wait(), timeout=1.0)
            # Barge-in triggered: mark activity and clear to allow next loop
            state['last_activity_ms'] = int(time.time() * 1000)
            log_event("bot_barge_in_handled", session_id=session_id)
            stop_event.clear()
        except asyncio.TimeoutError:
            pass

    log_event("bot_exit", session_id=session_id or "")

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
        log_event("bot_exit")
        sys.exit(1)
