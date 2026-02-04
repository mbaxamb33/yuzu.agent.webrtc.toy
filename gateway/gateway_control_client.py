import asyncio
import os
from typing import Optional, Callable

try:
    from . import gateway_control_pb2 as gw
    from . import gateway_control_pb2_grpc as gw_grpc
except Exception:
    # Fallback to absolute import if package-relative fails
    import gateway_control_pb2 as gw
    import gateway_control_pb2_grpc as gw_grpc


def _grpc_error_info(e: Exception) -> str:
    """Extract useful info from gRPC errors."""
    if hasattr(e, 'code') and callable(e.code):
        code = e.code()
        # Get just the name of the status code (e.g., "INTERNAL" instead of "StatusCode.INTERNAL")
        code_name = code.name if hasattr(code, 'name') else str(code)
        details = e.details() if hasattr(e, 'details') and callable(e.details) else ''
        debug = ''
        if hasattr(e, 'debug_error_string') and callable(e.debug_error_string):
            debug = e.debug_error_string()[:200]
        return f"{code_name}:{details or debug}"
    return str(e)[:200]


class GatewayControlClient:
    """Async gRPC client for Orchestrator control stream.

    Exposes helpers to send events (session_open, feature, transcripts, tts)
    and receives commands (arm_barge_in, start/stop mic→STT, start/stop TTS).
    """

    def __init__(self, session_id: str, loop: asyncio.AbstractEventLoop, log: Callable, stop_event: asyncio.Event, state: dict | None = None):
        self.session_id = session_id or ""
        self._loop = loop
        self._log = log
        self._stop_event = stop_event
        self._state = state if state is not None else {}
        self._channel = None
        self._stub = None
        self._call = None
        self._recv_task = None
        self._write_task = None
        self._feature_task = None
        self._reconnect_task = None
        self._write_queue: asyncio.Queue = None  # Will be created in connect()
        self._closed = False
        self._reconnecting = False
        self._room_url_last: Optional[str] = None
        # Feature coalescing (rate limit to ~10Hz)
        self._feature_latest: Optional[float] = None
        self._feature_last_sent: Optional[float] = None
        self._feature_interval_sec: float = float(os.environ.get('ORCH_FEATURE_INTERVAL_SEC', '0.1'))
        # Optional callbacks that gateway wires
        self.on_start_tts: Optional[Callable[[str], asyncio.Future]] = None

    async def connect(self):
        from grpc import aio
        target = os.environ.get('ORCH_ADDR', 'localhost:9090')
        self._channel = aio.insecure_channel(target)
        self._stub = gw_grpc.GatewayControlStub(self._channel)
        self._call = self._stub.Session()
        self._write_queue = asyncio.Queue()
        self._recv_task = self._loop.create_task(self._recv_loop())
        self._write_task = self._loop.create_task(self._write_loop())
        self._feature_task = self._loop.create_task(self._feature_loop())
        # Reconnect supervisor
        if self._reconnect_task is None:
            self._reconnect_task = self._loop.create_task(self._reconnect_supervisor())

    async def _write_loop(self):
        """Serialize all writes through a single coroutine to avoid races."""
        try:
            while not self._closed:
                try:
                    # Wait for next message with timeout to check closed flag
                    msg = await asyncio.wait_for(self._write_queue.get(), timeout=1.0)
                except asyncio.TimeoutError:
                    continue
                if msg is None:  # Shutdown signal
                    break
                if self._call is None:
                    # Drop non-critical telemetry silently; log once for critical types
                    continue
                try:
                    # Identify message type for logging
                    which = None
                    try:
                        which = msg.WhichOneof('evt')
                    except Exception:
                        which = None
                    await self._call.write(msg)
                except Exception as e:
                    # Log first write error
                    if not getattr(self, '_write_error_logged', False):
                        self._write_error_logged = True
                        self._log("orchestrator_write_error", session_id=self.session_id,
                                 metrics={"error": _grpc_error_info(e), "which": which or "unknown"})
                    # Emit specific errors for known types to preserve existing dashboards
                    err_tag = _grpc_error_info(e)
                    if which == 'feature':
                        self._log("orchestrator_feature_send_failed", session_id=self.session_id,
                                  metrics={"error": err_tag, "successful_sends_before_fail": int(self._state.get('features_sent_ok', 0))})
                    elif which == 'tts':
                        self._log("orchestrator_tts_event_failed", session_id=self.session_id,
                                  metrics={"type": getattr(getattr(msg, 'tts', None), 'type', ''), "error": err_tag})
                    elif which == 'transcript_final':
                        self._log("orchestrator_transcript_send_error", session_id=self.session_id,
                                  metrics={"error": err_tag})
                    # Mark call as dead so we stop trying
                    self._call = None
                    # Trigger reconnect supervisor
                    self._reconnecting = False  # allow supervisor to kick in immediately
                    break
        except asyncio.CancelledError:
            pass

    async def close(self):
        self._closed = True
        # Signal write loop to stop
        if self._write_queue is not None:
            try:
                self._write_queue.put_nowait(None)
            except Exception:
                pass
        # Cancel tasks
        if self._write_task is not None:
            self._write_task.cancel()
            try:
                await self._write_task
            except Exception:
                pass
        if self._feature_task is not None:
            self._feature_task.cancel()
            try:
                await self._feature_task
            except Exception:
                pass
        if self._reconnect_task is not None:
            self._reconnect_task.cancel()
            try:
                await self._reconnect_task
            except Exception:
                pass
        try:
            if self._call is not None:
                await self._call.done_writing()
        except Exception:
            pass
        try:
            if self._channel is not None:
                await self._channel.close()
        except Exception:
            pass

    def _enqueue(self, msg):
        """Thread-safe: enqueue a message for the write loop."""
        if self._write_queue is None or self._closed:
            return False
        try:
            self._write_queue.put_nowait(msg)
            return True
        except Exception:
            return False

    async def send_session_open(self, room_url: str):
        if self._closed:
            return
        self._room_url_last = room_url
        ev = gw.GatewayEvent(session_id=self.session_id, session_open=gw.SessionOpen(session_id=self.session_id, room_url=room_url))
        self._enqueue(ev)

    async def send_feature(self, rms: float):
        """Coalesce features to a 10Hz loop. Store latest RMS; writer will send."""
        if self._closed:
            return
        try:
            self._feature_latest = float(rms)
        except Exception:
            pass

    async def send_transcript_interim(self, utterance_id: str, text: str):
        if self._closed or self._call is None:
            return
        ev = gw.GatewayEvent(session_id=self.session_id, transcript_interim=gw.TranscriptInterim(utterance_id=utterance_id, text=text))
        self._enqueue(ev)

    async def send_transcript_final(self, utterance_id: str, text: str):
        if self._closed or self._call is None:
            return
        ev = gw.GatewayEvent(session_id=self.session_id, transcript_final=gw.TranscriptFinal(utterance_id=utterance_id, text=text))
        if self._enqueue(ev):
            self._log("orchestrator_transcript_queued", session_id=self.session_id, metrics={"text_len": len(text)})

    async def send_tts_event(self, typ: str, reason: str = "", first_audio_ms: int | None = None):
        if self._closed:
            self._log("orchestrator_tts_event_call_none", session_id=self.session_id, metrics={"type": typ})
            return
        evt = gw.TTSEvent(type=typ)
        if reason:
            evt.reason = reason
        if first_audio_ms is not None:
            evt.first_audio_ms = int(first_audio_ms)
        ev = gw.GatewayEvent(session_id=self.session_id, tts=evt)
        if self._enqueue(ev):
            self._log("orchestrator_tts_event_queued", session_id=self.session_id, metrics={"type": typ})

    async def _recv_loop(self):
        try:
            while True:
                cmd = await self._call.read()
                if cmd is None:
                    # End-of-stream: server closed its send side, the call is finished.
                    # Mark the call as unusable so subsequent writes return early
                    # instead of failing with "RPC already finished".
                    self._log("orchestrator_stream_closed", session_id=self.session_id)
                    self._call = None
                    return
                which = cmd.WhichOneof('cmd')
                if which == 'arm_barge_in':
                    guard = int(getattr(cmd.arm_barge_in, 'guard_ms', 0) or 0)
                    min_rms = int(getattr(cmd.arm_barge_in, 'min_rms', 0) or 0)
                    # Update local thresholds for backward-compat logs
                    if guard > 0:
                        self._state['local_stop_guard_ms'] = guard
                    if min_rms > 0:
                        self._state['local_stop_min_rms'] = min_rms
                    self._log("orchestrator_arm_barge_in", session_id=self.session_id, metrics={"guard_ms": guard, "min_rms": min_rms})
                elif which == 'start_mic_to_stt' or which == 'stop_mic_to_stt':
                    enabled = (which == 'start_mic_to_stt')
                    self._state['mic_to_stt_enabled'] = enabled
                    self._log("orchestrator_mic_to_stt", session_id=self.session_id, metrics={"enabled": enabled})
                elif which == 'stop_tts':
                    self._log("orchestrator_stop_tts", session_id=self.session_id)
                    try:
                        self._stop_event.set()
                    except Exception:
                        pass
                elif which == 'start_tts':
                    if callable(self.on_start_tts):
                        try:
                            await self.on_start_tts(cmd.start_tts.text)
                        except Exception as e:
                            self._log("gateway_tts_start_error", session_id=self.session_id, metrics={"error": str(e)})
                else:
                    # ack / join_room / unknown
                    pass
        except asyncio.CancelledError:
            return
        except Exception as e:
            self._log("orchestrator_control_error", session_id=self.session_id, metrics={"error": _grpc_error_info(e)})
            # Mark call as unusable on error
            self._call = None

    async def _feature_loop(self):
        """Periodic sender for coalesced features at ~10Hz (configurable)."""
        try:
            interval = max(0.05, float(self._feature_interval_sec))
            while not self._closed:
                await asyncio.sleep(interval)
                if self._call is None:
                    continue
                v = self._feature_latest
                if v is None:
                    continue
                # Only send if changed significantly or enough time passed
                if self._feature_last_sent is None or abs(v - self._feature_last_sent) >= 1.0:
                    ev = gw.GatewayEvent(session_id=self.session_id, feature=gw.Feature(rms=float(v)))
                    if self._enqueue(ev):
                        # Track successful enqueue to help diagnose send failures
                        self._state['features_sent_ok'] = int(self._state.get('features_sent_ok', 0)) + 1
                    self._feature_last_sent = v
        except asyncio.CancelledError:
            return

    async def _reconnect_supervisor(self):
        """Keep the control stream connected; reconnect with backoff when _call is None."""
        try:
            backoff = 0.2
            while not self._closed:
                await asyncio.sleep(0.1)
                if self._call is not None:
                    backoff = 0.2  # reset on healthy
                    continue
                # Need to reconnect
                if self._reconnecting:
                    continue
                self._reconnecting = True
                try:
                    from grpc import aio
                    target = os.environ.get('ORCH_ADDR', 'localhost:9090')
                    # Reuse channel if possible
                    if self._channel is None:
                        self._channel = aio.insecure_channel(target)
                    self._stub = gw_grpc.GatewayControlStub(self._channel)
                    call = self._stub.Session()
                    # Swap in
                    self._call = call
                    # Start a fresh recv loop
                    self._recv_task = self._loop.create_task(self._recv_loop())
                    # Re-send session_open if we have it
                    if self._room_url_last:
                        ev = gw.GatewayEvent(session_id=self.session_id, session_open=gw.SessionOpen(session_id=self.session_id, room_url=self._room_url_last))
                        self._enqueue(ev)
                    self._log("orchestrator_reconnected", session_id=self.session_id)
                    backoff = 0.2
                except Exception as e:
                    self._log("orchestrator_reconnect_failed", session_id=self.session_id, metrics={"error": _grpc_error_info(e), "backoff_ms": int(backoff*1000)})
                    await asyncio.sleep(backoff)
                    backoff = min(backoff * 2.0, 5.0)
                finally:
                    self._reconnecting = False
        except asyncio.CancelledError:
            return
