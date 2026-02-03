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
        # Optional callbacks that gateway wires
        self.on_start_tts: Optional[Callable[[str], asyncio.Future]] = None

    async def connect(self):
        from grpc import aio
        target = os.environ.get('ORCH_ADDR', 'localhost:9090')
        self._channel = aio.insecure_channel(target)
        self._stub = gw_grpc.GatewayControlStub(self._channel)
        self._call = self._stub.Session()
        self._recv_task = self._loop.create_task(self._recv_loop())

    async def close(self):
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

    async def send_session_open(self, room_url: str):
        if not self._call:
            return
        ev = gw.GatewayEvent(session_id=self.session_id, session_open=gw.SessionOpen(session_id=self.session_id, room_url=room_url))
        await self._call.write(ev)

    async def send_feature(self, rms: float):
        if not self._call:
            return
        try:
            await self._call.write(gw.GatewayEvent(session_id=self.session_id, feature=gw.Feature(rms=float(rms))))
        except Exception:
            pass

    async def send_transcript_interim(self, utterance_id: str, text: str):
        if not self._call:
            return
        await self._call.write(gw.GatewayEvent(session_id=self.session_id, transcript_interim=gw.TranscriptInterim(utterance_id=utterance_id, text=text)))

    async def send_transcript_final(self, utterance_id: str, text: str):
        if not self._call:
            return
        await self._call.write(gw.GatewayEvent(session_id=self.session_id, transcript_final=gw.TranscriptFinal(utterance_id=utterance_id, text=text)))

    async def send_tts_event(self, typ: str, reason: str = "", first_audio_ms: int | None = None):
        if not self._call:
            return
        evt = gw.TTSEvent(type=typ)
        if reason:
            evt.reason = reason
        if first_audio_ms is not None:
            evt.first_audio_ms = int(first_audio_ms)
        await self._call.write(gw.GatewayEvent(session_id=self.session_id, tts=evt))

    async def _recv_loop(self):
        try:
            while True:
                cmd = await self._call.read()
                if cmd is None:
                    await asyncio.sleep(0.05)
                    continue
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
            self._log("orchestrator_control_error", session_id=self.session_id, metrics={"error": str(e)})
