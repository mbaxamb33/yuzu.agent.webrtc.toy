import time
from collections import deque
from dataclasses import dataclass
from typing import Optional

import numpy as np
from scipy.signal import resample_poly


def now_mono_ms() -> int:
    return int(time.monotonic() * 1000)


def downsample_48k_to_16k(pcm48: bytes) -> bytes:
    if not pcm48:
        return b""
    x = np.frombuffer(pcm48, dtype=np.int16).astype(np.float64)
    y = resample_poly(x, up=1, down=3)
    y_i16 = np.clip(y, -32768, 32767).astype(np.int16)
    return y_i16.tobytes()


@dataclass
class Frame:
    data: bytes
    ts_ms: int
    seq: int


class RingBuffer:
    def __init__(self, capacity_ms: int = 300, hard_cap_ms: int = 500, frame_ms: int = 20):
        self.frame_ms = int(frame_ms)
        self.capacity_frames = max(1, int(capacity_ms // frame_ms))
        self.hard_cap_frames = max(self.capacity_frames, int(hard_cap_ms // frame_ms))
        self._q: deque[Frame] = deque()
        self._seq = 0

    def push(self, frame_bytes: bytes):
        self._seq += 1
        self._q.append(Frame(frame_bytes, now_mono_ms(), self._seq))
        while len(self._q) > self.hard_cap_frames:
            self._q.popleft()

    def flush_all(self) -> bytes:
        if not self._q:
            return b""
        out = b"".join(f.data for f in self._q)
        self._q.clear()
        return out


class FrameBatcher:
    def __init__(self, batch_ms: int = 100):
        self.batch_ms = int(batch_ms)
        self._bytes_per_ms = 32  # 16kHz mono * 2 bytes
        self._target = self.batch_ms * self._bytes_per_ms
        self._buf = bytearray()

    def add(self, pcm16k: bytes):
        if pcm16k:
            self._buf.extend(pcm16k)

    def emit_ready(self) -> Optional[bytes]:
        if len(self._buf) >= self._target:
            chunk = bytes(self._buf[: self._target])
            del self._buf[: self._target]
            return chunk
        return None

    def flush(self) -> Optional[bytes]:
        if not self._buf:
            return None
        chunk = bytes(self._buf)
        self._buf.clear()
        return chunk

    def set_batch_ms(self, batch_ms: int):
        batch_ms = max(20, int(batch_ms))
        self.batch_ms = batch_ms
        self._target = self.batch_ms * self._bytes_per_ms

