"""Client for the v1 control-plane protocol exposed by the Go server.

Phase 1 covers pairing and heartbeat:

- register: the server validates the model hash and answers with a session,
  the model description and the initial execution policy (always LOCAL);
- heartbeat: every ``heartbeat_interval_ms`` the client sends the telemetry
  of the last window and receives the current policy.

Usage::

    uv run --project client python -m sciot_client.control_plane_client \
        --server http://127.0.0.1:8080 \
        --device-id web_96 \
        --model-dir ~/sciot-models/yolo_small_320
"""

from __future__ import annotations

import argparse
import math
import threading
from collections.abc import Sequence
from dataclasses import dataclass, field
from pathlib import Path

import requests

from sciot_client import segments

PROTOCOL_VERSION = 1
REGISTER_PATH = "/api/v1/register"
HEARTBEAT_PATH = "/api/v1/heartbeat"

MODE_LOCAL = "LOCAL"
MODE_SPLIT = "SPLIT"


# ---------------------------------------------------------------------------
# Errors
# ---------------------------------------------------------------------------


class PairingError(Exception):
    """Base class for every pairing failure."""


class ServerUnreachable(PairingError):
    """The server could not be reached (connection error, timeout)."""


class ProtocolError(PairingError):
    """The server answered with an error body ``{"error": {code, message}}``."""

    def __init__(self, status: int, code: str, message: str):
        super().__init__(f"{status} {code}: {message}")
        self.status = status
        self.code = code
        self.message = message


class ModelNotFound(ProtocolError):
    """The server does not know this model_hash: run LOCAL-only."""


class UnsupportedProtocol(ProtocolError):
    """Client and server speak different protocol versions."""


class SessionExpired(ProtocolError):
    """The server no longer knows this session: fall back to LOCAL and
    register again (server restart, or a newer registration of this device)."""


# Error code (from the server contract) -> exception class.
ERROR_CLASSES: dict[str, type[ProtocolError]] = {
    "model_not_found": ModelNotFound,
    "unsupported_protocol": UnsupportedProtocol,
    "session_expired": SessionExpired,
}


# ---------------------------------------------------------------------------
# Contract types
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class TensorSpec:
    shape: tuple[int, ...]  # batch included (always 1)
    dtype: str  # "float32", ...

    @classmethod
    def from_dict(cls, data: dict) -> TensorSpec:
        return cls(tuple(int(d) for d in data["shape"]), str(data["dtype"]))


@dataclass(frozen=True)
class Postprocess:
    """How to decode the model output into detections (same as the server)."""

    decoder: str  # "yolo_pro", "fomo"
    labels: tuple[str, ...]
    score_threshold: float
    max_detections: int
    iou_threshold: float | None  # None for decoders without NMS

    @classmethod
    def from_dict(cls, data: dict) -> Postprocess:
        iou = data.get("iou_threshold")  # absent by contract when there is no NMS
        return cls(
            str(data["decoder"]),
            tuple(str(label) for label in data["labels"]),
            float(data["score_threshold"]),
            int(data["max_detections"]),
            None if iou is None else float(iou),
        )


@dataclass(frozen=True)
class ModelInfo:
    model_key: str
    num_layers: int
    last_offloading_layer: int
    input: TensorSpec
    postprocess: Postprocess

    @classmethod
    def from_dict(cls, data: dict) -> ModelInfo:
        # Keys are read explicitly: a missing key raises instead of silently
        # becoming a default.
        return cls(
            data["model_key"],
            int(data["num_layers"]),
            int(data["last_offloading_layer"]),
            TensorSpec.from_dict(data["input"]),
            Postprocess.from_dict(data["postprocess"]),
        )


@dataclass(frozen=True)
class Policy:
    mode: str
    split_point: int | None
    policy_version: int
    # SPLIT window: frames in flight at most. None = the stream's maximum
    # (max_inflight of the hello); always None in LOCAL.
    max_inflight: int | None = None

    @classmethod
    def from_dict(cls, data: dict) -> Policy:
        # The client must not apply a policy it does not understand: failing
        # loudly is safer than guessing.
        mode = data["mode"]
        # No int() here: None is valid in LOCAL, and coercing "5" or 5.0
        # would hide a contract violation.
        split_point = data["split_point"]
        policy_version = int(data["policy_version"])
        window = data.get("max_inflight")

        if mode == MODE_LOCAL:
            if split_point is not None:
                raise ValueError(f"LOCAL policy must have split_point None, got {split_point!r}")
            if window is not None:
                raise ValueError(f"LOCAL policy must have no max_inflight, got {window!r}")
            return cls(mode, None, policy_version)

        if mode == MODE_SPLIT:
            # bool is a subclass of int: JSON true must not pass as layer 1.
            if not isinstance(split_point, int) or isinstance(split_point, bool):
                raise ValueError(f"SPLIT policy requires an int split_point, got {split_point!r}")
            if window is not None and (
                not isinstance(window, int) or isinstance(window, bool) or window < 1
            ):
                raise ValueError(f"max_inflight must be an int >= 1, got {window!r}")
            return cls(mode, split_point, policy_version, window)

        raise ValueError(f"unknown policy mode {mode!r}")


@dataclass(frozen=True)
class Session:
    """Result of a successful registration."""

    session_id: str
    model: ModelInfo
    policy: Policy
    heartbeat_interval_ms: int
    # The server has the stream: open it right after registering, in every
    # mode. Absent (older servers) = False; SPLIT still opens it on demand.
    data_plane: bool = False

    @classmethod
    def from_dict(cls, data: dict) -> Session:
        # Build a Session from the whole 200 response body, using
        #   ModelInfo.from_dict and Policy.from_dict for the nested objects.
        #   An empty session_id is invalid: raise ValueError.
        session_id = data["session_id"]
        if not session_id:
            raise ValueError("session_id must not be empty")
        return cls(
            session_id,
            ModelInfo.from_dict(data["model"]),
            Policy.from_dict(data["policy"]),
            int(data["heartbeat_interval_ms"]),
            bool(data.get("data_plane", False)),
        )


# ---------------------------------------------------------------------------
# Telemetry
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class WindowStats:
    """Inference statistics of one heartbeat window."""

    count: int
    mean_ms: float | None  # None when count == 0
    max_ms: float | None  # None when count == 0
    # Per segment index: (sum of ms, runs) of the window; only segments run.
    segments: dict[int, tuple[float, int]] = field(default_factory=dict)
    # Time the SplitPipeline waited for a place in the window of frames in
    # flight (docs/CLIENT.md, "Window full"); sent as wait_ms when > 0.
    wait_ms: float = 0.0


class InferenceWindow:
    """Accumulates inference times between two heartbeats.

    The inference loop calls ``record()`` after every inference; the heartbeat
    thread calls ``snapshot_and_reset()`` once per interval. The two run in
    different threads, so every access to the accumulators holds the lock
    (the Python counterpart of the Go Store's sync.Mutex).

    Memory is constant (three numbers, plus a sum and a count per segment),
    whatever the inference rate: the same design works on the ESP32.
    """

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._count = 0
        self._sum_ms = 0.0
        self._max_ms = 0.0
        self._segments: dict[int, tuple[float, int]] = {}
        self._wait_ms = 0.0

    def record(self, inference_time_ms: float) -> None:
        """Add one inference to the current window."""
        # A negative duration means a clock bug: better to fail in the loop
        # than to send it to the server.
        # NaN passes a "< 0" check (every comparison with NaN is False) and
        # would be serialized as the non-JSON token NaN: reject it explicitly.
        if not math.isfinite(inference_time_ms) or inference_time_ms < 0:
            raise ValueError(f"invalid inference time: {inference_time_ms}")
        with self._lock:
            self._count += 1
            self._sum_ms += inference_time_ms
            self._max_ms = max(self._max_ms, inference_time_ms)

    def record_segments(self, first: int, seconds: Sequence[float]) -> None:
        """Add one run of segments first, first+1, ... (prefix of a frame or
        completion of a refused one): every run counts."""
        ms = [s * 1000 for s in seconds]
        if not all(math.isfinite(m) and m >= 0 for m in ms):
            raise ValueError(f"invalid segment times: {seconds}")
        with self._lock:
            for offset, m in enumerate(ms):
                total, runs = self._segments.get(first + offset, (0.0, 0))
                self._segments[first + offset] = (total + m, runs + 1)

    def record_wait(self, ms: float) -> None:
        """Add a wait for a place in the window of frames in flight."""
        if not math.isfinite(ms) or ms < 0:
            raise ValueError(f"invalid wait: {ms}")
        with self._lock:
            self._wait_ms += ms

    def snapshot_and_reset(self) -> WindowStats:
        """Return the stats of the current window and start a new one.

        Reading and resetting happen under the SAME lock acquisition: if they
        were two separate steps, an inference recorded in between would be
        lost (counted in neither window).
        """
        with self._lock:
            count, sum_ms, max_ms = self._count, self._sum_ms, self._max_ms
            segments, self._segments = self._segments, {}
            wait_ms, self._wait_ms = self._wait_ms, 0.0
            self._count = 0
            self._sum_ms = 0.0
            self._max_ms = 0.0
        # Outside the lock: no need to block the inference loop for arithmetic.
        if count == 0:
            return WindowStats(0, None, None, segments, wait_ms)
        return WindowStats(count, sum_ms / count, max_ms, segments, wait_ms)


@dataclass(frozen=True)
class Telemetry:
    """Telemetry sent in one heartbeat. Optional metrics default to None."""

    window: WindowStats
    cpu_load: float | None = None  # 0..1
    rtt_ms: float | None = None
    memory_available_bytes: int | None = None
    # Segments of the session's model: segment_ms / segment_count are sent
    # with one entry each. None (no session yet) = not sent.
    num_segments: int | None = None

    def to_dict(self) -> dict:
        """Serialize following the v1 contract.

        - ``inference_count`` is always present;
        - ``inference_time_ms`` / ``inference_time_max_ms`` only if count > 0;
        - optional metrics only if not None.
        """
        data: dict = {"inference_count": self.window.count}
        if self.window.count > 0:
            data["inference_time_ms"] = self.window.mean_ms
            data["inference_time_max_ms"] = self.window.max_ms
        # "is not None", never truthiness: a legitimate 0.0 must still be sent
        # (the same absent-vs-zero distinction Go solves with pointers).
        if self.cpu_load is not None:
            data["cpu_load"] = self.cpu_load
        if self.rtt_ms is not None:
            data["rtt_ms"] = self.rtt_ms
        if self.memory_available_bytes is not None:
            data["memory_available_bytes"] = self.memory_available_bytes
        segments = self.window.segments
        if self.num_segments is not None and segments:
            if max(segments) >= self.num_segments or min(segments) < 0:
                raise ValueError(f"segment {max(segments)} outside the model's {self.num_segments}")
            runs = [segments.get(i, (0.0, 0)) for i in range(self.num_segments)]
            data["segment_ms"] = [total / n if n else None for total, n in runs]
            data["segment_count"] = [n for _, n in runs]
        # Absent = 0 by contract (a client that never waits): sent only when > 0.
        if self.window.wait_ms > 0:
            data["wait_ms"] = self.window.wait_ms
        return data


# ---------------------------------------------------------------------------
# Client
# ---------------------------------------------------------------------------


class ControlPlaneClient:
    """Talks to the Go server's control plane over a persistent HTTP session.

    ``requests.Session`` keeps the TCP connection alive between calls, which
    is what the server's IdleTimeout (120 s) is tuned for. It is injectable so
    tests can pass a fake.
    """

    def __init__(
        self,
        base_url: str,
        device_id: str,
        *,
        device_type: str = "python",
        http: requests.Session | None = None,
        timeout: float = 5.0,
    ):
        self.base_url = base_url.rstrip("/")
        self.device_id = device_id
        self.device_type = device_type
        self.http = http if http is not None else requests.Session()
        self.timeout = timeout

    def register(
        self, model_hash: str, *, probe: bool = False, wait_for_window: bool = False
    ) -> Session:
        """Register this device and return the session assigned by the server.

        ``probe``: declare the capability (this client answers probe_request
        on the stream). ``wait_for_window``: with its window full this client
        waits for a place (docs/CLIENT.md, "Window full"); the server's cost
        model simulates that rule. Declare only what the code does: the server
        asks for nothing that is not declared.

        Raises:
            ServerUnreachable: network error or timeout.
            ModelNotFound / UnsupportedProtocol / ProtocolError: error answer.
            ValueError: 200 answer whose body does not respect the contract.
        """
        payload = {
            "protocol_version": PROTOCOL_VERSION,
            "device_id": self.device_id,
            "device_type": self.device_type,
            "model_hash": model_hash,
            "capabilities": {"probe": probe, "wait_for_window": wait_for_window},
        }
        return Session.from_dict(self._post_json(REGISTER_PATH, payload))

    def heartbeat(self, session: Session, applied: Policy, telemetry: Telemetry) -> Policy:
        """Send one heartbeat and return the server's current policy.

        ``applied`` is the policy the client is applying right now: the server
        uses it as an acknowledgement. Applying the returned policy (only if
        its version is newer) is the caller's job, not this method's.

        Raises:
            SessionExpired: the server does not know this session any more.
            ServerUnreachable / ProtocolError: as for register.
            ValueError: 200 answer whose policy does not respect the contract.
        """
        payload = {
            "device_id": self.device_id,
            "session_id": session.session_id,
            "policy_version": applied.policy_version,
            "mode": applied.mode,
            "telemetry": telemetry.to_dict(),
        }
        body = self._post_json(HEARTBEAT_PATH, payload)
        return Policy.from_dict(body["policy"])

    def _post_json(self, path: str, payload: dict) -> dict:
        """POST payload to path and return the decoded 200 body.

        Common part of every control-plane call (the Python counterpart of
        decodeJSON on the Go side): network errors become ServerUnreachable,
        non-200 answers become the matching ProtocolError.
        """
        url = self.base_url + path
        try:
            response = self.http.post(url, json=payload, timeout=self.timeout)
        except requests.RequestException as exc:
            raise ServerUnreachable(f"cannot reach {url}: {exc}") from exc

        if response.status_code != 200:
            self._raise_for_error(response)
        return response.json()

    @staticmethod
    def _raise_for_error(response: requests.Response) -> None:
        """Turn an error response into the matching ProtocolError subclass."""
        try:
            error = response.json()["error"]
            code = error["code"]
            message = error["message"]
        except (ValueError, KeyError, TypeError):
            # Proxy error page, empty body, or JSON that is not the contract.
            raise ProtocolError(
                response.status_code, "unexpected_response", response.text[:200]
            ) from None

        error_class = ERROR_CLASSES.get(code, ProtocolError)
        raise error_class(response.status_code, code, message)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Pair with the SCIoT Go server.")
    parser.add_argument("--server", default="http://127.0.0.1:8080")
    parser.add_argument("--device-id", required=True)
    parser.add_argument("--model-dir", required=True)
    args = parser.parse_args(argv)

    model_hash = segments.model_hash(Path(args.model_dir).expanduser())
    client = ControlPlaneClient(args.server, args.device_id)
    try:
        session = client.register(model_hash)
    except ModelNotFound:
        print(f"server does not know model {model_hash}: running LOCAL-only")
        return 2
    except PairingError as exc:
        print(f"pairing failed: {exc}")
        return 1

    print(f"paired: session={session.session_id}")
    print(f"  model:  {session.model}")
    print(f"  policy: {session.policy}")
    print(f"  heartbeat every {session.heartbeat_interval_ms} ms")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
