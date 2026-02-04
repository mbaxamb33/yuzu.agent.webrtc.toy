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
        self._addr = os.environ.get('TTS_ADDR', 'localhost:9093')
        self._channel = None
        self._stub = None

    async def fetch_pcm48k(self, session_id: str, voice_id: str, text: str) -> bytes:
        from grpc import aio
        self._log('tts_fetch_start', session_id=session_id, metrics={'text_len': len(text), 'addr': self._addr})
        try:
            self._channel = aio.insecure_channel(self._addr)
            self._stub = tts_grpc.TTSStub(self._channel)
            call = self._stub.Session()
            await call.write(tts.ClientMessage(start=tts.StartRequest(session_id=session_id, request_id='req', voice_id=voice_id, text=text)))
            pcm = bytearray()
            chunk_count = 0
            # Timeouts and limits
            read_timeout = float(os.environ.get('TTS_READ_TIMEOUT_SEC', '5.0'))
            total_timeout = float(os.environ.get('TTS_TOTAL_TIMEOUT_SEC', '30.0'))
            max_bytes = int(os.environ.get('TTS_MAX_BYTES', str(10 * 1024 * 1024)))  # 10MB default
            start_mono = asyncio.get_running_loop().time()
            try:
                while True:
                    # Per-read deadline
                    try:
                        resp = await asyncio.wait_for(call.read(), timeout=read_timeout)
                    except asyncio.TimeoutError:
                        # Abort on read stall
                        self._log('tts_fetch_timeout', session_id=session_id, metrics={'chunks': chunk_count, 'bytes': len(pcm), 'timeout_s': read_timeout})
                        break
                    # End of stream check - grpc.aio may return None or EOF sentinel or unexpected type
                    if resp is None:
                        self._log('tts_fetch_eof_none', session_id=session_id, metrics={'chunks': chunk_count, 'bytes': len(pcm)})
                        break
                    if not isinstance(resp, tts.ServerMessage):
                        self._log('tts_fetch_eof_sentinel', session_id=session_id, metrics={'chunks': chunk_count, 'bytes': len(pcm), 'type': type(resp).__name__})
                        break
                    which = resp.WhichOneof('msg')
                    if not which:
                        # Empty message or stream ended
                        self._log('tts_fetch_eof_empty_msg', session_id=session_id, metrics={'chunks': chunk_count, 'bytes': len(pcm)})
                        break
                    if which == 'connected':
                        self._log('tts_fetch_connected', session_id=session_id)
                        continue
                    if which == 'audio':
                        pcm.extend(resp.audio.pcm48k)
                        chunk_count += 1
                        # Backpressure/memory guard
                        if len(pcm) > max_bytes:
                            self._log('tts_fetch_truncated', session_id=session_id, metrics={'chunks': chunk_count, 'bytes': len(pcm), 'max_bytes': max_bytes})
                            break
                    elif which == 'error':
                        self._log('tts_fetch_error', session_id=session_id, metrics={'msg': resp.error.message})
                        break
                    else:
                        self._log('tts_fetch_unknown', session_id=session_id, metrics={'which': which})
                        break
                    # Global timeout check
                    if (asyncio.get_running_loop().time() - start_mono) > total_timeout:
                        self._log('tts_fetch_total_timeout', session_id=session_id, metrics={'chunks': chunk_count, 'bytes': len(pcm), 'timeout_s': total_timeout})
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
        except Exception as e:
            # Extract gRPC error details
            if hasattr(e, 'code') and callable(e.code):
                err_info = f"{e.code().name}: {e.details() if hasattr(e, 'details') else ''}"
            else:
                err_info = str(e)[:200]
            self._log('tts_fetch_exception', session_id=session_id, metrics={'error': err_info, 'addr': self._addr})
            return b''
