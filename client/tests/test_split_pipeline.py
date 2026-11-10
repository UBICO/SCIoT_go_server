"""SplitPipeline with a fake stream client: no network, no TensorFlow."""

from __future__ import annotations

import csv
import dataclasses
import io
from typing import ClassVar

import numpy as np
import pytest

from sciot_client import control_plane_client as cpc
from sciot_client import data_plane_client as dpc
from sciot_client.split_pipeline import (
    FrameCSV,
    FrameRecord,
    FrameTable,
    SegmentTimes,
    SplitPipeline,
)

LAST = 7
SPLIT = cpc.Policy("SPLIT", 4, 2)
LOCAL = cpc.Policy("LOCAL", None, 2)
POSTPROCESS = cpc.Postprocess("yolo_pro", ("bicycle", "stop sign"), 0.5, 200, 0.5)

# A model output with one bicycle: what the fake segments return.
ONE_BICYCLE = np.array([[[0, 0, 0.5, 0.5, 0.875, 0]]], dtype=np.float32)


def session(sid: str = "S1") -> cpc.Session:
    model = cpc.ModelInfo("yolo", 8, LAST, cpc.TensorSpec((1, 320, 320, 3), "float32"), POSTPROCESS)
    return cpc.Session(sid, model, SPLIT, 1000)


class FakeHeartbeat:
    def __init__(self, sess=None):
        self.session = sess if sess is not None else session()
        self.offered = []

    def offer_policy(self, policy):
        self.offered.append(policy)


class FakeClient:
    """Stands in for DataPlaneClient; the test triggers the callbacks."""

    instances: ClassVar[list] = []

    def __init__(
        self,
        server,
        device_id,
        session_id,
        *,
        on_result,
        on_error,
        on_policy,
        on_closed,
        accept=True,
        fail_open=False,
    ):
        self.session_id = session_id
        self.on_result, self.on_error, self.on_closed = on_result, on_error, on_closed
        self.accept, self.fail_open = accept, fail_open
        self.sent: list[tuple[dpc.Pending, bytes]] = []
        self.windows: list[int | None] = []  # the window passed to each send
        self.closed = False
        self.close_calls = 0
        self.waits: list[float] = []  # timeout of every wait_for_slot
        FakeClient.instances.append(self)

    def open(self):
        if self.fail_open:
            raise OSError("connection refused")
        return dpc.Hello(3, 1 << 20, cpc.Policy("SPLIT", 4, 5), {4: 16})

    # Window-full rule: the test sets eta (seconds to a free place) and
    # free_at (test clock time the place frees; None = never). wait_for_slot
    # advances the test clock like a real wait.
    clock = None
    eta: float | None = None
    free_at: float | None = None

    def slot_eta_s(self, window, rtt_ms):
        return None if self.accept else self.eta

    def wait_for_slot(self, window, timeout_s):
        self.waits.append(timeout_s)
        if self.free_at is not None and self.clock.t + timeout_s >= self.free_at:
            self.clock.t = max(self.clock.t, self.free_at)
            self.accept, self.free_at = True, None
            return True
        self.clock.t += timeout_s
        return False

    def send(self, pending, payload, window=None):
        if not self.accept:
            return False
        self.sent.append((pending, payload))
        self.windows.append(window)
        return True

    @property
    def inflight(self):
        return 0

    def close(self):
        self.close_calls += 1
        self.closed = True


class Clock:
    def __init__(self):
        self.t = 0.0

    def __call__(self):
        return self.t


def make(hb=None, *, accept=True, fail_open=False, deadline_ms=None, clock=None):
    FakeClient.instances = []
    runs = []

    def run_segments(x, first, last):
        runs.append((first, last))
        return ONE_BICYCLE, [0.001] * (last - first + 1)

    def factory(*args, **kwargs):
        return FakeClient(*args, accept=accept, fail_open=fail_open, **kwargs)

    out = io.StringIO()
    FakeClient.clock = clock = clock or Clock()
    p = SplitPipeline(
        server="http://srv",
        device_id="web_96",
        heartbeat=hb or FakeHeartbeat(),
        last_layer=LAST,
        run_segments=run_segments,
        segment_times=SegmentTimes(),
        outputs=[FrameTable(out)],
        deadline_ms=deadline_ms,
        client_factory=factory,
        clock=clock,
        window=cpc.InferenceWindow(),
    )
    return p, runs, out


ACT = np.zeros((1, 4), dtype=np.float32)  # 16 bytes, the size in the fake hello


# --- SegmentTimes ------------------------------------------------------------------


def test_segment_times_feed_the_window():
    window = cpc.InferenceWindow()
    times = SegmentTimes(window=window)
    times.record(3, [0.002, 0.004])
    assert window.snapshot_and_reset().segments == {
        3: pytest.approx((2.0, 1)),
        4: pytest.approx((4.0, 1)),
    }


def test_segment_times():
    t = SegmentTimes(alpha=0.5)
    assert t.remaining_ms(4, LAST) is None  # never measured
    t.record(0, [0.001] * 8)  # a LOCAL run: 8 segments of 1 ms
    assert t.remaining_ms(4, LAST) == pytest.approx(3.0)  # segments 5, 6, 7
    t.record(5, [0.003, 0.003, 0.003])
    assert t.remaining_ms(4, LAST) == pytest.approx(6.0)  # EWMA 0.5: 2 ms each
    assert t.remaining_ms(LAST, LAST) is None  # nothing left: no deadline


# --- LOCAL and SPLIT -----------------------------------------------------------------


def test_local_frame_is_decoded_here():
    p, _runs, out = make()
    p.handle_output(LOCAL, LAST, ONE_BICYCLE, "T", 1.0, 30.0)
    assert p.counts["local"] == 1 and not FakeClient.instances
    assert "LOCAL" in out.getvalue() and "  1 | local" in out.getvalue()


def test_local_opens_the_stream_when_the_server_has_one():
    sess = dataclasses.replace(session(), data_plane=True)
    p, runs, _out = make(FakeHeartbeat(sess))
    p.handle_output(LOCAL, LAST, ONE_BICYCLE, "T", 1.0, 30.0)
    p.handle_output(LOCAL, LAST, ONE_BICYCLE, "T", 1.0, 30.0)
    # One stream, opened once, no frame sent: pings and probes run on it.
    (client,) = FakeClient.instances
    assert not client.sent and p.counts["local"] == 2 and not runs


def test_split_frame_is_sent_without_waiting():
    hb = FakeHeartbeat()
    p, runs, out = make(hb)
    p.handle_output(SPLIT, 4, ACT, "2026-10-21T10:00:00.000Z", 1.0, 10.0)

    (client,) = FakeClient.instances
    ((pending, payload),) = client.sent
    assert pending.sequence_id == 1 and pending.split_point == 4 and pending.policy_version == 2
    assert pending.captured_at == "2026-10-21T10:00:00.000Z" and len(payload) == 16
    assert pending.deadline_ms is None  # segments 5..7 never measured here
    assert hb.offered == [cpc.Policy("SPLIT", 4, 5)], "la policy dell'hello va al HeartbeatLoop"
    assert p.counts["sent"] == 1 and not runs and out.getvalue() == ""

    # The result arrives on the receiver thread, is handled by drain().
    client.on_result(
        dpc.FrameResult(
            pending,
            {"timing": {"server_ms": 8.0, "receive_ms": 3.2}, "detections": [{}, {}]},
            12.5,
        )
    )
    p.drain()
    row = out.getvalue().splitlines()[-1]
    assert p.counts["result"] == 1
    assert "SPLIT" in row and "8.0" in row and "4.5" in row and "|   2 | result" in row
    assert "|     3.2 |" in row, "receive_ms del server nella colonna Recv ms"


def test_deadline_from_segment_times_or_fixed():
    p, _, _ = make()
    p.segment_times.record(0, [0.002] * 8)
    p.handle_output(SPLIT, 4, ACT, "T", 0, 0)
    assert FakeClient.instances[0].sent[0][0].deadline_ms == pytest.approx(6.0)

    p, _, _ = make(deadline_ms=45.0)
    p.handle_output(SPLIT, 4, ACT, "T", 0, 0)
    assert FakeClient.instances[0].sent[0][0].deadline_ms == 45.0


def test_window_full_finishes_locally():
    p, runs, out = make(accept=False)
    p.handle_output(SPLIT, 4, ACT, "T", 1.0, 10.0)
    assert runs == [(5, LAST)], "il device esegue i segmenti 5..7 dall'attivazione"
    assert p.counts["window_full"] == 1 and p.counts["finished_locally"] == 1
    assert "local (window_full)" in out.getvalue()
    assert p.segment_times.remaining_ms(4, LAST) == pytest.approx(3.0), "e misura quei segmenti"


@pytest.mark.parametrize(
    "code", ["server_busy", "deadline_exceeded", "worker_unavailable", "stream_closed"]
)
def test_refused_frame_finishes_locally(code):
    p, runs, out = make()
    p.handle_output(SPLIT, 4, ACT, "T", 1.0, 10.0)
    client = FakeClient.instances[0]
    pending = client.sent[0][0]
    client.on_error(dpc.FrameError(pending, code, "msg"))
    p.drain()
    assert runs == [(5, LAST)]
    assert p.counts[code] == 1 and f"local ({code})" in out.getvalue()


def test_no_session_runs_locally():
    hb = FakeHeartbeat()
    hb.session = None
    p, runs, _out = make(hb)
    p.handle_output(SPLIT, 4, ACT, "T", 1.0, 10.0)
    assert p.counts["no_stream"] == 1 and runs == [(5, LAST)] and not FakeClient.instances


# --- stream lifecycle ------------------------------------------------------------------


def test_open_failure_retries_with_backoff():
    clock = Clock()
    p, _runs, _ = make(fail_open=True, clock=clock)
    p.handle_output(SPLIT, 4, ACT, "T", 0, 0)
    p.handle_output(SPLIT, 4, ACT, "T", 0, 0)
    assert len(FakeClient.instances) == 1, "nessun nuovo tentativo prima dell'attesa"
    assert p.counts["no_stream"] == 2

    clock.t = 1.0  # first wait: 1 s
    p.handle_output(SPLIT, 4, ACT, "T", 0, 0)
    assert len(FakeClient.instances) == 2
    clock.t = 2.5  # second wait: 2 s, not elapsed yet
    p.handle_output(SPLIT, 4, ACT, "T", 0, 0)
    assert len(FakeClient.instances) == 2


def test_new_session_reopens_the_stream():
    hb = FakeHeartbeat()
    p, _, _ = make(hb)
    p.handle_output(SPLIT, 4, ACT, "T", 0, 0)
    old = FakeClient.instances[0]
    old.on_closed(dpc.CLOSE_SESSION_EXPIRED)
    p.drain()
    hb.session = session("S2")  # the heartbeat registered again
    p.handle_output(SPLIT, 4, ACT, "T", 0, 0)
    assert old.closed and FakeClient.instances[-1].session_id == "S2"
    assert FakeClient.instances[-1].sent[0][0].sequence_id == 2  # still increasing


def test_replaced_stream_is_not_reopened():
    p, _runs, _ = make()
    p.handle_output(SPLIT, 4, ACT, "T", 0, 0)
    client = FakeClient.instances[0]
    client.closed = True
    client.on_closed(dpc.CLOSE_REPLACED)
    p.drain()
    p.handle_output(SPLIT, 4, ACT, "T", 0, 0)
    assert len(FakeClient.instances) == 1 and p.counts["no_stream"] == 1


def test_going_away_retries_later():
    clock = Clock()
    p, _, _ = make(clock=clock)
    p.handle_output(SPLIT, 4, ACT, "T", 0, 0)
    client = FakeClient.instances[0]
    client.closed = True
    client.on_closed(1001)
    p.drain()
    p.handle_output(SPLIT, 4, ACT, "T", 0, 0)
    assert len(FakeClient.instances) == 1, "aspetta prima di riaprire"
    clock.t = 1.0
    p.handle_output(SPLIT, 4, ACT, "T", 0, 0)
    assert len(FakeClient.instances) == 2


# --- table ---------------------------------------------------------------------------


def record(seq=1, **kw):
    fields = {
        "t_s": 1.0,
        "captured_at": "T",
        "seq": seq,
        "policy_version": 3,
        "mode": "SPLIT",
        "split": 4,
        "acq_ms": 1.0,
        "age_ms": 0.0,
        "dev_ms": 10.0,
        "wait_ms": 0.0,
        "srv_ms": 8.0,
        "recv_ms": 3.2,
        "net_ms": 4.5,
        "rtt_ms": 12.5,
        "sent_bytes": 16,
        "detections": 1,
        "latency_ms": 25.0,
        "outcome": "result",
    }
    fields.update(kw)
    return FrameRecord(**fields)


def test_frame_table_repeats_the_header():
    out = io.StringIO()
    table = FrameTable(out)
    for i in range(21):
        table.write(record(i))
    text = out.getvalue()
    assert text.count("Outcome") == 2
    assert "—" not in text.splitlines()[3]
    table.write(
        record(
            None,
            mode="LOCAL",
            split=None,
            srv_ms=None,
            recv_ms=None,
            net_ms=None,
            detections=None,
            outcome="local",
        )
    )
    assert out.getvalue().splitlines()[-1].count("—") == 6


def test_frames_csv(tmp_path):
    path = tmp_path / "frames.csv"
    out = FrameCSV(path)
    out.write(record(42))
    out.write(
        record(
            None,
            mode="LOCAL",
            split=None,
            srv_ms=None,
            recv_ms=None,
            net_ms=None,
            rtt_ms=None,
            sent_bytes=0,
            outcome="local",
        )
    )
    # Flushed at every row: readable while the client runs.
    rows = list(csv.DictReader(path.open()))
    out.close()
    assert tuple(rows[0]) == FrameCSV.COLUMNS
    assert (
        rows[0]["seq"] == "42" and rows[0]["policy_version"] == "3" and rows[0]["rtt_ms"] == "12.5"
    )
    assert rows[1]["seq"] == "" and rows[1]["srv_ms"] == "" and rows[1]["outcome"] == "local"


def test_records_carry_version_bytes_and_time():
    clock = Clock()
    p, _, _ = make(clock=clock)
    seen = []

    class Sink:
        def write(self, r):
            seen.append(r)

        def close(self):
            pass

    p.outputs = [Sink()]
    clock.t = 2.5
    p.handle_output(SPLIT, 4, ACT, "CAP", 1.0, 10.0)
    client = FakeClient.instances[0]
    pending = client.sent[0][0]
    clock.t = 3.0
    client.on_result(
        dpc.FrameResult(pending, {"timing": {"server_ms": 8.0}, "detections": []}, 12.5)
    )
    p.drain()
    p.handle_output(LOCAL, LAST, ONE_BICYCLE, "CAP2", 1.0, 30.0)
    split, local = seen
    assert (split.t_s, split.policy_version, split.sent_bytes, split.rtt_ms) == (
        3.0,
        2,
        16,
        12.5,
    )
    assert split.captured_at == "CAP" and split.outcome == "result"
    # At arrival: acq 1 + dev 10 + rtt 12.5, not when drain() ran (0.5 s later).
    assert split.latency_ms == pytest.approx(23.5)
    # A LOCAL frame: acquisition → now, i.e. acq + dev.
    assert local.latency_ms == pytest.approx(31.0)
    assert (local.sent_bytes, local.policy_version, local.mode) == (0, 2, "LOCAL")


def test_latency_starts_at_the_capture():
    # A camera frame existed age_ms before acquire(): the latency includes it.
    clock = Clock()
    p, _, _ = make(clock=clock)
    seen = []

    class Sink:
        def write(self, r):
            seen.append(r)

        def close(self):
            pass

    p.outputs = [Sink()]
    clock.t = 2.5
    p.handle_output(SPLIT, 4, ACT, "CAP", 1.0, 10.0, 4.0)
    client = FakeClient.instances[0]
    client.on_result(
        dpc.FrameResult(client.sent[0][0], {"timing": {"server_ms": 8.0}, "detections": []}, 12.5)
    )
    p.drain()
    p.handle_output(LOCAL, LAST, ONE_BICYCLE, "CAP2", 1.0, 30.0, -0.5)
    split, local = seen
    assert split.latency_ms == pytest.approx(4.0 + 1.0 + 10.0 + 12.5)
    assert local.latency_ms == pytest.approx(-0.5 + 1.0 + 30.0)  # arrived during acquire()
    assert (split.age_ms, local.age_ms) == (4.0, -0.5)


def test_split_minus_one_sends_the_model_input():
    # -1: the device ran nothing; the payload is the model input itself.
    p, _runs, _ = make()
    image = np.zeros((1, 4), dtype=np.float32)  # 16 bytes, like the fake hello's size
    p.segment_times.record(0, [0.001] * 8)
    p.handle_output(cpc.Policy("SPLIT", -1, 3), -1, image, "T", 1.0, 0.0)
    pending, payload = FakeClient.instances[0].sent[0]
    assert pending.split_point == -1 and len(payload) == 16
    assert pending.deadline_ms == pytest.approx(8.0), "tutti gli 8 segmenti restano da fare"


def test_the_policy_window_reaches_send():
    p, _runs, _ = make()
    x = np.zeros((1, 4), dtype=np.float32)
    p.handle_output(cpc.Policy("SPLIT", 4, 3, 2), 4, x, "T", 1.0, 0.0)
    p.handle_output(cpc.Policy("SPLIT", 4, 4), 4, x, "T", 1.0, 0.0)
    assert FakeClient.instances[0].windows == [2, None], "None: the hello's maximum"


# --- window full: wait for a place (docs/CLIENT.md, "Window full") ---------------


def slow_device(rtt_ms=200.0):
    """A pipeline whose device takes 10 ms per segment 0..4 and 100 ms per
    segment 5..7 (X = 300 ms at split 4), with one result of rtt_ms seen, and
    the window now full."""
    p, runs, out = make()
    p.segment_times.record(0, [0.01] * 5 + [0.1] * 3)
    p.handle_output(SPLIT, 4, ACT, "T", 10.0, 50.0)  # acq 10 ms
    client = FakeClient.instances[0]
    client.on_result(dpc.FrameResult(client.sent[0][0], {"timing": {"server_ms": 50.0}}, rtt_ms))
    p.drain()
    p.window.snapshot_and_reset()
    client.accept = False
    return p, runs, out, client


def test_full_window_waits_after_the_prefix_when_shorter(tmp_path):
    p, runs, _out, client = slow_device()
    path = tmp_path / "frames.csv"
    p.outputs.append(FrameCSV(path))
    client.eta, client.free_at = 0.1, p.clock() + 0.1  # a place in 100 ms < X 300
    p.handle_output(SPLIT, 4, ACT, "T", 10.0, 50.0)
    assert len(client.sent) == 2 and not runs, "sent after the wait, not finished here"
    assert p.counts["sent_after_wait"] == 1 and p.counts["window_full"] == 0
    assert client.waits == [pytest.approx(0.3)], "waits at most X"
    assert p.window.snapshot_and_reset().wait_ms == pytest.approx(100.0)
    client.on_result(dpc.FrameResult(client.sent[1][0], {"timing": {"server_ms": 50.0}}, 200.0))
    p.drain()
    p.outputs[-1].close()
    (row,) = csv.DictReader(path.open())
    assert float(row["wait_ms"]) == pytest.approx(100.0)
    # Latency: acq 10 + prefix 50 + wait 100 + rtt 200.
    assert float(row["latency_ms"]) == pytest.approx(360.0)


def test_full_window_finishes_locally_when_the_wait_is_longer():
    p, runs, _out, client = slow_device()
    client.eta, client.free_at = 0.5, p.clock() + 0.5  # 500 ms >= X 300
    p.handle_output(SPLIT, 4, ACT, "T", 10.0, 50.0)
    assert not client.waits and runs == [(5, LAST)] and p.counts["window_full"] == 1


def test_full_window_wait_past_x_finishes_locally():
    p, runs, _out, client = slow_device()
    client.eta, client.free_at = 0.1, None  # expected in 100 ms, never answered
    p.handle_output(SPLIT, 4, ACT, "T", 10.0, 50.0)
    assert client.waits == [pytest.approx(0.3)] and runs == [(5, LAST)]
    assert p.counts["window_full"] == 1
    assert p.window.snapshot_and_reset().wait_ms == pytest.approx(300.0)


def test_no_wait_without_a_measured_round_trip():
    p, runs, _out = make(accept=False)
    p.segment_times.record(0, [0.01] * 5 + [0.1] * 3)
    p.handle_output(SPLIT, 4, ACT, "T", 10.0, 50.0)
    p.before_acquire(SPLIT, 4)
    assert not FakeClient.instances[0].waits and runs == [(5, LAST)]


def test_before_acquire_waits_until_the_frame_would_be_ready():
    p, _runs, _out, client = slow_device()
    client.eta, client.free_at = 0.2, p.clock() + 0.2  # place in 200 ms
    p.before_acquire(SPLIT, 4)
    # W − A − P = 200 − 10 − 50 = 140 ms: the next frame is ready when the place frees.
    assert client.waits == [pytest.approx(0.14)]
    assert p.window.snapshot_and_reset().wait_ms == pytest.approx(140.0)


def test_before_acquire_returns_early_when_a_place_frees():
    p, _runs, _out, client = slow_device()
    client.eta, client.free_at = 0.2, p.clock() + 0.05  # answered earlier than expected
    p.before_acquire(SPLIT, 4)
    assert p.window.snapshot_and_reset().wait_ms == pytest.approx(50.0)


def test_before_acquire_does_not_wait_in_local_or_when_longer_than_x():
    p, _runs, _out, client = slow_device()
    client.eta = 0.5  # 500 ms >= X 300: the frame will be finished here anyway
    p.before_acquire(SPLIT, 4)
    p.before_acquire(LOCAL, LAST)
    assert not client.waits
