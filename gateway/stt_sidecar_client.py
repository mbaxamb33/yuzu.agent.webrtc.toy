import asyncio
import os
from typing import Optional, Callable

try:
    from . import stt_pb2 as stt
    from . import stt_pb2_grpc as stt_grpc
except Exception:
    import stt_pb2 as stt
    import stt_pb2_grpc as stt_grpc


class STTSidecarClient:
    def __init__(self, session_id: Optional[str], loop: asyncio.AbstractEventLoop, log: Callable, ws_queue: Optional[asyncio.Queue] = None, state: Optional[dict] = None):
        self.session_id = session_id or ""
        self._loop = loop
        self._log = log
        self._ws_queue = ws_queue
        self._state = state if state is not None else {}
        self._channel = None
        self._stub = None
        self._call = None
        self._recv_task = None
        self.enabled = True
        self._orch = None
        # Write lock to serialize gRPC stream writes (only one write allowed at a time)
        self._write_lock = asyncio.Lock()
        # metrics
        self.bytes_sent = 0
        self.frames_sent = 0

    async def preconnect(self):
        from grpc import aio
        uds = os.environ.get('STT_UDS_PATH', '/run/app/stt.sock')
        target = f"unix://{uds}" if not uds.startswith('unix://') else uds
        self._channel = aio.insecure_channel(target)
        self._stub = stt_grpc.STTStub(self._channel)
        self._call = self._stub.Session()
        self._recv_task = self._loop.create_task(self._recv_loop())
        self._log("stt_connected", session_id=self.session_id)

    def attach_orchestrator(self, orch_client):
        self._orch = orch_client

    async def start_utterance(self, utterance_id: str):
        if not self._call:
            return
        msg = stt.ClientMessage(start=stt.ControlStart(session_id=self.session_id, worker_id="", utterance_id=utterance_id, language="en-US", sample_rate=16000, protocol_version="1"))
        async with self._write_lock:
            await self._call.write(msg)
        self._log("stt_utterance_start", session_id=self.session_id, utterance_id=utterance_id)

    async def send_audio(self, pcm16k_bytes: bytes):
        if not self._call or not pcm16k_bytes:
            return
        dur_ms = int(len(pcm16k_bytes) / 32)
        async with self._write_lock:
            await self._call.write(stt.ClientMessage(audio=stt.AudioChunk(pcm16k=pcm16k_bytes, duration_ms=dur_ms)))
        self.bytes_sent += len(pcm16k_bytes)
        self.frames_sent += 1
        if self.frames_sent % 10 == 0:
            self._log("stt_audio_sent", session_id=self.session_id, metrics={"frames": self.frames_sent, "bytes": self.bytes_sent})

    async def end_utterance(self):
        if not self._call:
            return
        async with self._write_lock:
            await self._call.write(stt.ClientMessage(drain=stt.Drain()))
        self._log("stt_utterance_end", session_id=self.session_id)

    async def close(self):
        try:
            if self._call:
                await self._call.done_writing()
        except Exception:
            pass
        try:
            if self._channel:
                await self._channel.close()
        except Exception:
            pass

    async def _recv_loop(self):
        try:
            while True:
                resp = await self._call.read()
                if resp is None:
                    await asyncio.sleep(0.02)
                    continue
                which = resp.WhichOneof('msg')
                if which == 'interim':
                    text = resp.interim.text
                    # Update recent interim state for local-stop dual-signal gating
                    try:
                        self._state['stt_last_interim_ts_ms'] = int(asyncio.get_running_loop().time() * 1000)
                        self._state['stt_last_interim_len'] = len(text)
                    except Exception:
                        pass
                    if self._orch is not None:
                        try:
                            await self._orch.send_transcript_interim(resp.interim.utterance_id, text)
                        except Exception:
                            pass
                    self._log("stt_transcript_interim", session_id=self.session_id, metrics={"chars": len(text)})
                elif which == 'final':
                    text = resp.final.text
                    self._log("stt_transcript_final", session_id=self.session_id, metrics={"chars": len(text), "text": text[:100] if text else ""})
                    if self._orch is not None:
                        try:
                            self._log("stt_sending_to_orchestrator", session_id=self.session_id, metrics={"utterance_id": resp.final.utterance_id, "text_len": len(text)})
                            await self._orch.send_transcript_final(resp.final.utterance_id, text)
                            self._log("stt_sent_to_orchestrator", session_id=self.session_id)
                        except Exception as e:
                            self._log("stt_orchestrator_send_error", session_id=self.session_id, metrics={"error": str(e)})
                    else:
                        self._log("stt_no_orchestrator_attached", session_id=self.session_id)
                elif which == 'error':
                    self._log("stt_error", session_id=self.session_id, metrics={"code": getattr(resp.error, 'enum_code', 0), "msg": resp.error.message})
                else:
                    # connected/metrics/pong ignored here
                    pass
        except asyncio.CancelledError:
            return
        except Exception as e:
            self._log("stt_error", session_id=self.session_id, metrics={"error": str(e)})
