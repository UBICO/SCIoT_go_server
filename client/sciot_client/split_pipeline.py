"""SPLIT execution for the Go client: frames to the server, results back.

The inference loop (http_go_client.run_loop) computes segments 0..k and hands
the activation to ``SplitPipeline.handle_output``. In SPLIT the pipeline
sends it on the data plane stream and returns at once: the loop acquires the
next frame while this one is on the server. Answers arrive on the stream's
receiver thread, which only puts them in a queue; ``drain()``, called by the
loop at the start of every cycle, handles them on the main thread.

Choices (docs/GO_CONTROL_PLANE.md, "Data plane (SPLIT)"):

- never wait for the server: a full window, a missing stream or a refused
  frame all mean the device finishes the frame itself, from the activation
  it kept (segments k+1..last);
- the deadline of a frame is the time the device would need to finish it
  alone (mean time of segments k+1..last on this device): a later result is
  worse than running LOCAL. Unknown until those segments ran here once.
"""

from __future__ import annotations

import csv
import logging
import os
import queue
import time
from collections import Counter
from collections.abc import Callable, Sequence
from dataclasses import asdict, dataclass, fields
from datetime import UTC, datetime
from typing import Any, TextIO

import numpy as np

from sciot_client import decoders
from sciot_client.control_plane_client import MODE_LOCAL, InferenceWindow, Policy
from sciot_client.data_plane_client import (
    CLOSE_REPLACED,
    CLOSE_SESSION_EXPIRED,
    DataPlaneClient,
    FrameError,
    FrameResult,
    Pending,
)

logger = logging.getLogger(__name__)

MIN_RETRY_S = 1.0
MAX_RETRY_S = 30.0


def utc_now() -> str:
    """Wall-clock time for captured_at: RFC 3339, UTC, milliseconds."""
    return datetime.now(UTC).isoformat(timespec="milliseconds").replace("+00:00", "Z")


class SegmentTimes:
    """Mean compute time of each segment on this device (EWMA, in ms).

    ``window``, if given, also receives every run: the heartbeat sends the
    window's per-segment times to the server (segment_ms / segment_count).
    """

    def __init__(self, alpha: float = 0.2, window: InferenceWindow | None = None) -> None:
        self.alpha = alpha
        self._ms: dict[int, float] = {}
        self._window = window

    def record(self, first: int, seconds: Sequence[float]) -> None:
        """Times of segments first, first+1, ... as measured by one run."""
        if self._window is not None:
            self._window.record_segments(first, seconds)
        for offset, s in enumerate(seconds):
            i, sample = first + offset, s * 1000
            prev = self._ms.get(i)
            self._ms[i] = sample if prev is None else self.alpha * sample + (1 - self.alpha) * prev

    def remaining_ms(self, after: int, last: int) -> float | None:
        """Time of segments after+1..last; None if one was never measured."""
        times = [self._ms.get(i) for i in range(after + 1, last + 1)]
        if not times or any(t is None for t in times):
            return None
        return float(sum(times))


# --- outputs ----------------------------------------------------------------------


@dataclass(frozen=True)
class FrameRecord:
    """The outcome of one frame, as the device saw it."""

    t_s: float  # when the outcome was known, seconds since the pipeline started
    captured_at: str
    seq: int | None  # sequence_id; None for a frame never sent
    policy_version: int  # the policy the frame was computed with
    mode: str  # LOCAL or SPLIT
    split: int | None
    acq_ms: float
    # Age of the frame when acquire() started: how long it existed before the
    # loop asked for it (a camera frame waiting in frames.LatestFrame); negative
    # if it arrived while acquire() waited for it; 0 for the static image.
    age_ms: float
    dev_ms: float  # device compute: prefix, plus the completion if finished here
    srv_ms: float | None
    # The server's reading of the frame, first to last byte (timing.receive_ms):
    # the upload without the latency, possibly underestimated (TCP buffering).
    recv_ms: float | None
    net_ms: float | None  # round trip minus server time
    rtt_ms: float | None  # send → result, on the device
    sent_bytes: int  # payload sent on the stream (0 if never sent)
    detections: int | None
    # Capture → outcome on the device (age + acquisition + compute + ...), waits
    # included (the answer of a refused frame, its local completion): what an
    # application sees.
    latency_ms: float
    outcome: str  # local, result, local (<why>)


TABLE_HEADER = (
    "    Seq | Mode  | Split | Acq ms | Dev ms | Srv ms | Recv ms | Net ms | Det | Outcome"
)
TABLE_REPEAT = 20


def _opt(v: float | None, fmt: str) -> str:
    return "—" if v is None else format(v, fmt)


class FrameTable:
    """One row per frame, printed when its outcome is known (main thread only)."""

    def __init__(self, stream: TextIO | None = None) -> None:
        import sys

        self.stream = stream if stream is not None else sys.stdout
        self.rows = 0

    def write(self, r: FrameRecord) -> None:
        if self.rows % TABLE_REPEAT == 0:
            print(file=self.stream)
            print(TABLE_HEADER, file=self.stream)
            print("-" * len(TABLE_HEADER), file=self.stream)
        print(
            f"{_opt(r.seq, 'd'):>7} | {r.mode:<5} | {_opt(r.split, 'd'):>5} | "
            f"{r.acq_ms:>6.1f} | {r.dev_ms:>6.1f} | {_opt(r.srv_ms, '.1f'):>6} | "
            f"{_opt(r.recv_ms, '.1f'):>7} | {_opt(r.net_ms, '.1f'):>6} | {_opt(r.detections, 'd'):>3} | {r.outcome}",
            file=self.stream,
        )
        self.rows += 1

    def close(self) -> None:
        pass


class FrameCSV:
    """One CSV row per frame (--frames-csv), for the benchmarks.

    Same data as the table plus what a program needs: the policy version, to
    attribute every frame to the configuration it was computed with, and the
    time, to drop warm-up seconds and compute rates. Flushed at every row: a
    benchmark may read the file while the client runs.
    """

    COLUMNS = tuple(f.name for f in fields(FrameRecord))

    def __init__(self, path: str | os.PathLike) -> None:
        self._file = open(path, "w", newline="", encoding="utf-8")  # noqa: SIM115 (close())
        self._writer = csv.DictWriter(self._file, fieldnames=self.COLUMNS)
        self._writer.writeheader()

    def write(self, r: FrameRecord) -> None:
        row = {k: ("" if v is None else v) for k, v in asdict(r).items()}
        self._writer.writerow(row)
        self._file.flush()

    def close(self) -> None:
        self._file.close()


# --- pipeline -----------------------------------------------------------------------


@dataclass
class _Device:
    """What the device knows about a frame in flight (for its record)."""

    acq_ms: float
    dev_ms: float
    sent_bytes: int
    captured: float  # clock() when the frame was captured
    age_ms: float = 0.0  # see FrameRecord.age_ms


class SplitPipeline:
    """Sends SPLIT frames, handles their answers, finishes the failed ones."""

    def __init__(
        self,
        *,
        server: str,
        device_id: str,
        heartbeat: Any,  # HeartbeatLoop: .session, .offer_policy(policy)
        last_layer: int,
        run_segments: Callable[[Any, int, int], tuple[Any, list[float]]],
        segment_times: SegmentTimes,
        outputs: Sequence[FrameTable | FrameCSV] = (),
        deadline_ms: float | None = None,
        client_factory: Callable[..., DataPlaneClient] = DataPlaneClient,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self.server = server
        self.device_id = device_id
        self.hb = heartbeat
        self.last_layer = last_layer
        self.run_segments = run_segments
        self.segment_times = segment_times
        self.outputs = list(outputs)
        self.fixed_deadline_ms = deadline_ms
        self.client_factory = client_factory
        self.clock = clock

        self.counts: Counter[str] = Counter()
        self._started = clock()
        self._events: queue.Queue = queue.Queue()
        self._client: DataPlaneClient | None = None
        self._device: dict[int, _Device] = {}
        self._seq = 0
        self._retry_at = 0.0
        self._backoff = MIN_RETRY_S
        self._replaced = False

    # --- called by the inference loop -------------------------------------------

    def handle_output(
        self,
        policy: Policy,
        layer: int,
        output: Any,
        captured_at: str,
        acq_ms: float,
        dev_ms: float,
        age_ms: float = 0.0,
    ) -> None:
        """The loop computed segments 0..layer of a frame: finish or send it.

        ``age_ms``: how long the frame existed before acquire() started
        (FrameRecord.age_ms); the latency starts at the capture, not at acquire().
        """
        version = policy.policy_version
        # The frame was captured age_ms + acq_ms + dev_ms ago: the loop calls
        # this right after computing.
        captured = self.clock() - (age_ms + acq_ms + dev_ms) / 1000
        frame = _Device(acq_ms, dev_ms, 0, captured, age_ms=age_ms)
        if policy.mode == MODE_LOCAL or layer >= self.last_layer:
            # The stream stays open in LOCAL too when the server has one:
            # pings, policy messages and probes measure and reach this device.
            session = self.hb.session
            if session is not None and session.data_plane:
                self._stream()
            self.counts["local"] += 1
            self._emit(
                frame,
                captured_at=captured_at,
                seq=None,
                policy_version=version,
                mode="LOCAL",
                split=None,
                detections=self._decode(output),
                outcome="local",
            )
            return

        client = self._stream()
        if client is None:
            self.counts["no_stream"] += 1
            self._finish_locally(layer, output, captured_at, version, frame, "no_stream")
            return

        self._seq += 1
        deadline = self.fixed_deadline_ms
        if deadline is None:
            deadline = self.segment_times.remaining_ms(layer, self.last_layer)
        pending = Pending(self._seq, layer, policy.policy_version, output, captured_at, deadline)
        payload = np.ascontiguousarray(output, dtype=np.float32).tobytes()
        if not client.send(pending, payload, window=policy.max_inflight):
            self.counts["window_full"] += 1
            self._finish_locally(layer, output, captured_at, version, frame, "window_full")
            return
        self.counts["sent"] += 1
        frame.sent_bytes = len(payload)
        self._device[pending.sequence_id] = frame

    def drain(self) -> None:
        """Handle every answer received so far (main thread)."""
        while True:
            try:
                kind, event = self._events.get_nowait()
            except queue.Empty:
                return
            if kind == "result":
                self._handle_result(event)
            elif kind == "error":
                self._handle_error(event)
            elif kind == "closed":
                self._handle_closed(event)

    def finish(self, timeout_s: float = 2.0) -> None:
        """Wait for the frames in flight (up to timeout_s), then close."""
        end = self.clock() + timeout_s
        while self._client is not None and self._client.inflight and self.clock() < end:
            self.drain()
            time.sleep(0.01)
        if self._client is not None:
            self._client.close()  # still in flight: reported as stream_closed
        self.drain()
        for out in self.outputs:
            out.close()

    # --- stream lifecycle --------------------------------------------------------

    def _stream(self) -> DataPlaneClient | None:
        """The open stream of the current session, opening it if needed."""
        session = self.hb.session
        if session is None or self._replaced:
            return None
        client = self._client
        if client is not None and not client.closed and client.session_id == session.session_id:
            return client
        if client is not None:  # closed, or of an old session
            client.close()
            self._client = None
        if self.clock() < self._retry_at:
            return None

        client = self.client_factory(
            self.server,
            self.device_id,
            session.session_id,
            on_result=lambda r: self._events.put(("result", r)),
            on_error=lambda e: self._events.put(("error", e)),
            on_policy=self.hb.offer_policy,
            on_closed=lambda code: self._events.put(("closed", code)),
        )
        try:
            hello = client.open()
        # Any failure (refused 409/503, unreachable, timeout): frames run LOCAL.
        except Exception as exc:  # noqa: BLE001
            self._retry_later(f"cannot open the stream: {exc}")
            return None
        logger.info("data plane: stream open (window %d)", hello.max_inflight)
        self.hb.offer_policy(hello.policy)
        self._client = client
        self._backoff = MIN_RETRY_S
        return client

    def _retry_later(self, why: str) -> None:
        logger.warning("data plane: %s; retrying in %.0f s, frames run LOCAL", why, self._backoff)
        self._retry_at = self.clock() + self._backoff
        self._backoff = min(self._backoff * 2, MAX_RETRY_S)

    def _handle_closed(self, code: int | None) -> None:
        if code == CLOSE_REPLACED:
            # Another client opened a stream with this device id: fighting it
            # would close each other in turn. Stay LOCAL.
            self._replaced = True
            logger.error(
                "data plane: another stream replaced this one (same device id?): LOCAL only"
            )
        elif code == CLOSE_SESSION_EXPIRED:
            # The heartbeat registers again; _stream() opens a stream for the
            # new session as soon as it exists.
            logger.info("data plane: session expired, stream closed")
        else:
            self._retry_later(f"stream closed ({code})")

    # --- answers ------------------------------------------------------------------

    def _handle_result(self, r: FrameResult) -> None:
        self.counts["result"] += 1
        p = r.pending
        frame = self._device.pop(p.sequence_id, None) or self._unknown()
        timing = r.result.get("timing", {})
        srv = timing.get("server_ms")
        net = None if srv is None else max(0.0, r.rtt_ms - srv)
        detections = r.result.get("detections")
        self._emit(
            frame,
            captured_at=p.captured_at,
            seq=p.sequence_id,
            policy_version=p.policy_version,
            mode="SPLIT",
            split=p.split_point,
            srv_ms=srv,
            recv_ms=timing.get("receive_ms"),
            net_ms=net,
            rtt_ms=r.rtt_ms,
            detections=None if detections is None else len(detections),
            outcome="result",
            # At ARRIVAL (the receiver measured rtt), not now: this thread may
            # have been busy finishing another frame locally meanwhile.
            latency_ms=frame.age_ms + frame.acq_ms + frame.dev_ms + r.rtt_ms,
        )

    def _handle_error(self, e: FrameError) -> None:
        self.counts[e.code] += 1
        if e.code not in ("server_busy", "deadline_exceeded", "stream_closed"):
            logger.warning("data plane: frame %d: %s: %s", e.pending.sequence_id, e.code, e.message)
        p = e.pending
        frame = self._device.pop(p.sequence_id, None) or self._unknown()
        self._finish_locally(
            p.split_point,
            p.activation,
            p.captured_at,
            p.policy_version,
            frame,
            e.code,
            seq=p.sequence_id,
        )

    def _finish_locally(
        self,
        layer: int,
        activation: Any,
        captured_at: str,
        policy_version: int,
        frame: _Device,
        why: str,
        seq: int | None = None,
    ) -> None:
        """Run segments layer+1..last here: the work already done is kept."""
        output, seconds = self.run_segments(activation, layer + 1, self.last_layer)
        self.segment_times.record(layer + 1, seconds)
        self.counts["finished_locally"] += 1
        frame.dev_ms += sum(seconds) * 1000
        self._emit(
            frame,
            captured_at=captured_at,
            seq=seq,
            policy_version=policy_version,
            mode="SPLIT",
            split=layer,
            detections=self._decode(output),
            outcome=f"local ({why})",
        )

    def _decode(self, output: Any) -> int | None:
        """Number of detections of a model output computed here."""
        session = self.hb.session
        if session is None:
            return None
        pp = session.model.postprocess
        params = decoders.Params(
            pp.decoder,
            pp.labels,
            pp.score_threshold,
            pp.max_detections,
            pp.iou_threshold,
        )
        return len(decoders.decode(params, np.asarray(output)))

    def _unknown(self) -> _Device:
        """A frame this pipeline did not send (should not happen): zeros."""
        return _Device(0.0, 0.0, 0, self.clock())

    def _emit(self, frame: _Device, **fields: Any) -> None:
        """Build the record of a frame and hand it to every output."""
        if not self.outputs:
            return
        now = self.clock()
        fields.setdefault("srv_ms", None)
        fields.setdefault("recv_ms", None)
        fields.setdefault("net_ms", None)
        fields.setdefault("rtt_ms", None)
        record = FrameRecord(
            t_s=now - self._started,
            acq_ms=frame.acq_ms,
            age_ms=frame.age_ms,
            dev_ms=frame.dev_ms,
            sent_bytes=frame.sent_bytes,
            **{"latency_ms": (now - frame.captured) * 1000, **fields},
        )
        for out in self.outputs:
            out.write(record)
