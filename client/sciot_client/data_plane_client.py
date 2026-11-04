"""Data plane client for the Go server: one WebSocket stream per device.

Protocol: docs/PROTOCOL.md of the Go server ("Data plane"). In short:

- the device opens ``GET /api/v1/stream`` with its device and session ids and
  receives a ``hello`` (window size, expected tensor sizes, current policy);
- a ``frame`` is a binary message: a u32 little-endian header length, a JSON
  header, the raw activation of segment ``split_point``;
- the server answers each frame with a ``result`` (binary, header =
  inference-result.v1) or an ``error`` (text), possibly out of order;
- it may also push a ``policy`` (text) when the policy changes.

Threads: the inference loop calls ``send`` and never waits for an answer
(frame N+1 is computed while N is on the server). A receiver thread reads the
stream and hands every answer to a callback. Callbacks run on the receiver
thread and must be quick: the Go client puts them in a queue that its main
thread drains.

Uses the synchronous API of ``websockets`` (websockets.sync.client): plain
blocking calls like ``requests``, safe to send from one thread while another
receives. The asyncio API would force the whole client, TensorFlow included,
into an event loop.
"""

from __future__ import annotations

import json
import logging
import struct
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any
from urllib.parse import urlsplit, urlunsplit

from websockets.exceptions import ConnectionClosed
from websockets.sync.client import connect as ws_connect

from sciot_client.control_plane_client import Policy

logger = logging.getLogger(__name__)

STREAM_PATH = "/api/v1/stream"
HEADER_DEVICE = "X-SCIoT-Device"
HEADER_SESSION = "X-SCIoT-Session"

# Close codes (docs/PROTOCOL.md, "Closing").
CLOSE_GOING_AWAY = 1001  # server shutting down: reconnect later
CLOSE_SESSION_EXPIRED = 4001  # registered again: this stream belongs to an old session
CLOSE_REPLACED = 4002  # another stream of the same device: do not fight it

# Error code given to frames in flight when the stream closes: not a server
# code, the device completes them LOCAL like any other failed frame.
STREAM_CLOSED = "stream_closed"

_HEADER_LENGTH = struct.Struct("<I")


def encode_binary(header: dict[str, Any], payload: bytes) -> bytes:
    """A binary message: u32 LE header length, JSON header, payload."""
    data = json.dumps(header, separators=(",", ":")).encode("utf-8")
    return _HEADER_LENGTH.pack(len(data)) + data + payload


def decode_binary(message: bytes) -> tuple[dict[str, Any], bytes]:
    """Split a binary message into its JSON header and payload."""
    if len(message) < _HEADER_LENGTH.size:
        raise ValueError(f"binary message of {len(message)} bytes: no header length")
    (length,) = _HEADER_LENGTH.unpack_from(message)
    end = _HEADER_LENGTH.size + length
    if end > len(message):
        raise ValueError(f"header length {length} exceeds the message ({len(message)} bytes)")
    return json.loads(message[_HEADER_LENGTH.size : end]), message[end:]


def stream_url(server: str) -> str:
    """http://host:port → ws://host:port/api/v1/stream (https → wss)."""
    parts = urlsplit(server)
    scheme = {"http": "ws", "https": "wss"}.get(parts.scheme, parts.scheme)
    return urlunsplit((scheme, parts.netloc, STREAM_PATH, "", ""))


@dataclass(frozen=True)
class Hello:
    max_inflight: int
    max_message_bytes: int
    policy: Policy
    input_bytes: dict[int, int]  # split point → bytes the server expects

    @classmethod
    def from_dict(cls, data: dict) -> Hello:
        if data.get("type") != "hello":
            raise ValueError(f"expected a hello message, got {data.get('type')!r}")
        return cls(
            int(data["max_inflight"]),
            int(data["max_message_bytes"]),
            Policy.from_dict(data["policy"]),
            {int(s["split_point"]): int(s["input_bytes"]) for s in data["segments"]},
        )


@dataclass
class Pending:
    """A frame in flight. ``activation`` is kept to finish it LOCAL if needed."""

    sequence_id: int
    split_point: int
    policy_version: int
    activation: Any  # numpy array, output of segment split_point
    captured_at: str
    deadline_ms: float | None
    sent_at: float = field(default=0.0)  # time.perf_counter() when sent


@dataclass(frozen=True)
class FrameResult:
    pending: Pending
    result: dict[str, Any]  # the inference-result.v1 header
    rtt_ms: float  # send → result received, measured on the device


@dataclass(frozen=True)
class FrameError:
    pending: Pending
    code: str  # server_busy, deadline_exceeded, ..., or STREAM_CLOSED
    message: str


class DataPlaneClient:
    """One open stream. Create, ``open()``, ``send()`` frames, ``close()``.

    A closed client is not reopened: the Go client creates a new one (with
    the current session id) when it needs the stream again.
    """

    def __init__(
        self,
        server: str,
        device_id: str,
        session_id: str,
        *,
        on_result: Callable[[FrameResult], None],
        on_error: Callable[[FrameError], None],
        on_policy: Callable[[Policy], None],
        on_closed: Callable[[int | None], None],
        connect: Callable[..., Any] = ws_connect,
        clock: Callable[[], float] = time.perf_counter,
    ) -> None:
        self.server = server
        self.device_id = device_id
        self.session_id = session_id
        self._on_result = on_result
        self._on_error = on_error
        self._on_policy = on_policy
        self._on_closed = on_closed
        self._connect = connect
        self._clock = clock

        self.hello: Hello | None = None
        self._ws: Any = None
        self._lock = threading.Lock()
        self._inflight: dict[int, Pending] = {}
        self._closed = False
        self._receiver: threading.Thread | None = None

    # --- lifecycle -------------------------------------------------------

    def open(self, timeout_s: float = 5.0) -> Hello:
        """Connect, read the hello and start the receiver thread.

        Raises what ``websockets`` raises on failure: InvalidStatus for an
        HTTP error before the upgrade (409 session_expired, 503 no data plane
        for this model), OSError when the server is unreachable.
        """
        self._ws = self._connect(
            stream_url(self.server),
            additional_headers={
                HEADER_DEVICE: self.device_id,
                HEADER_SESSION: self.session_id,
            },
            open_timeout=timeout_s,
        )
        try:
            self.hello = Hello.from_dict(json.loads(self._ws.recv(timeout=timeout_s)))
        except Exception:
            self._ws.close()
            raise
        self._receiver = threading.Thread(target=self._receive, name="data-plane-rx", daemon=True)
        self._receiver.start()
        return self.hello

    def close(self) -> None:
        """Close the stream; frames still in flight are reported as errors."""
        if self._ws is not None:
            self._ws.close()
        if self._receiver is not None and self._receiver is not threading.current_thread():
            self._receiver.join(timeout=5)

    @property
    def closed(self) -> bool:
        with self._lock:
            return self._closed

    @property
    def inflight(self) -> int:
        with self._lock:
            return len(self._inflight)

    # --- sending ---------------------------------------------------------

    def send(self, pending: Pending, payload: bytes, *, want_raw: bool = False) -> bool:
        """Send a frame without waiting for its answer.

        Returns False, without sending, when the window is full or the stream
        is closed: the caller then runs the frame LOCAL. Raises ValueError
        for a payload of the wrong size (a bug: the server would reject it).
        """
        assert self.hello is not None, "open() first"
        expected = self.hello.input_bytes.get(pending.split_point)
        if expected is None:
            raise ValueError(f"split point {pending.split_point} has no data plane")
        if len(payload) != expected:
            raise ValueError(
                f"split point {pending.split_point}: {len(payload)} bytes, server expects {expected}"
            )
        with self._lock:
            if self._closed or len(self._inflight) >= self.hello.max_inflight:
                return False
            # In the window BEFORE sending: the answer can arrive before
            # send() returns, and the receiver must find the frame.
            pending.sent_at = self._clock()
            self._inflight[pending.sequence_id] = pending

        header: dict[str, Any] = {
            "type": "frame",
            "sequence_id": pending.sequence_id,
            "policy_version": pending.policy_version,
            "split_point": pending.split_point,
            "encoding": "float32",
            "captured_at": pending.captured_at,
            "want_raw": want_raw,
        }
        if pending.deadline_ms is not None:
            header["deadline_ms"] = pending.deadline_ms
        try:
            self._ws.send(encode_binary(header, payload))
        except ConnectionClosed:
            # The receiver reports the frames in flight when it sees the
            # close; this one was never sent, so it is taken back here.
            with self._lock:
                self._inflight.pop(pending.sequence_id, None)
            return False
        return True

    # --- receiving -------------------------------------------------------

    def _take(self, sequence_id: int) -> Pending | None:
        with self._lock:
            return self._inflight.pop(sequence_id, None)

    def _receive(self) -> None:
        code: int | None = None
        try:
            while True:
                message = self._ws.recv()
                received = self._clock()
                if isinstance(message, str):
                    self._handle_text(json.loads(message))
                else:
                    self._handle_result(message, received)
        except ConnectionClosed as exc:
            code = exc.rcvd.code if exc.rcvd is not None else None
        except Exception:  # a bug in a handler must not leave frames hanging
            logger.exception("data plane: receiver failed")
            self._ws.close()
        finally:
            with self._lock:
                self._closed = True
                lost = list(self._inflight.values())
                self._inflight.clear()
            for pending in lost:
                self._on_error(FrameError(pending, STREAM_CLOSED, f"stream closed ({code})"))
            self._on_closed(code)

    def _handle_text(self, msg: dict[str, Any]) -> None:
        kind = msg.get("type")
        if kind == "error":
            pending = self._take(int(msg.get("sequence_id", 0)))
            if pending is None:
                logger.warning("data plane: error for an unknown frame: %s", msg)
                return
            self._on_error(FrameError(pending, str(msg.get("code")), str(msg.get("message", ""))))
        elif kind == "policy":
            self._on_policy(Policy.from_dict(msg["policy"]))
        elif kind == "probe_request":
            self._answer_probe(int(msg["probe_id"]), int(msg["bytes"]))
        else:
            logger.warning("data plane: unexpected message %r", kind)

    def _answer_probe(self, probe_id: int, size: int) -> None:
        """Send the probe from this receiver thread, at once: the server times
        its reception, so waiting for the inference loop would measure the
        device, not the network. websockets serializes concurrent sends."""
        header = {"type": "probe", "probe_id": probe_id}
        try:
            self._ws.send(encode_binary(header, bytes(size)))
        except ConnectionClosed:
            return  # the receive loop sees the close
        logger.info("data plane: probe %d answered (%d bytes)", probe_id, size)

    def _handle_result(self, message: bytes, received: float) -> None:
        header, _raw = decode_binary(message)
        pending = self._take(int(header["sequence_id"]))
        if pending is None:
            logger.warning("data plane: result for an unknown frame %s", header.get("sequence_id"))
            return
        self._on_result(FrameResult(pending, header, (received - pending.sent_at) * 1000))
