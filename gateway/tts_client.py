import asyncio
import os

try:
    from . import tts_pb2 as tts
    from . import tts_pb2_grpc as tts_grpc
except Exception:
    import tts_pb2 as tts
    import tts_pb2_grpc as tts_grpc


class TTSClient:
    def __init__(self, loop: asyncio.AbstractEventLoop, log):
        self._loop = loop
        self._log = log
        self._addr = os.environ.get('TTS_ADDR', ':9093')
        self._channel = None
        self._stub = None

    async def fetch_pcm48k(self, session_id: str, voice_id: str, text: str) -> bytes:
        from grpc import aio
        self._channel = aio.insecure_channel(self._addr)
        self._stub = tts_grpc.TTSStub(self._channel)
        call = self._stub.Session()
        await call.write(tts.ClientMessage(start=tts.StartRequest(session_id=session_id, request_id='req', voice_id=voice_id, text=text)))
        pcm = bytearray()
        try:
            while True:
                resp = await call.read()
                if resp is None:
                    await asyncio.sleep(0.01)
                    continue
                which = resp.WhichOneof('msg')
                if which == 'connected':
                    continue
                if which == 'audio':
                    pcm.extend(resp.audio.pcm48k)
                elif which == 'error':
                    self._log('tts_error', metrics={'msg': resp.error.message})
                    break
                else:
                    break
        finally:
            try:
                await call.done_writing()
            except Exception:
                pass
            try:
                await self._channel.close()
            except Exception:
                pass
        return bytes(pcm)

