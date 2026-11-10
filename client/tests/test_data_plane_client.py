"""DataPlaneClient against a fake stream server (websockets.sync.server)."""

from __future__ import annotations

import json
import queue
import threading
import time
from collections.abc import Callable

import pytest
from websockets.exceptions import ConnectionClosed, InvalidStatus
from websockets.sync.server import serve

from sciot_client import data_plane_client as dpc
from sciot_client.control_plane_client import Policy

SIZES = {-1: 12, 4: 8}  # split point → bytes, tiny tensors: the protocol is the same


def hello(max_inflight: int = 3) -> str:
    return json.dumps(
        {
            "type": "hello",
            "max_inflight": max_inflight,
            "max_message_bytes": 1 << 20,
            "policy": {"mode": "SPLIT", "split_point": 4, "policy_version": 2},
            "segments": [{"split_point": k, "input_bytes": v} for k, v in SIZES.items()],
        }
    )


def result(seq: int, **extra) -> bytes:
    header = {
        "schema": "inference-result.v1",
        "type": "result",
        "sequence_id": seq,
        "timing": {"server_ms": 1.5},
        "detections": [],
        **extra,
    }
    return dpc.encode_binary(header, b"")


class FakeServer:
    """A stream server running `script(ws)` for every connection."""

    def __init__(self, script: Callable, reject: int | None = None):
        self.headers = None  # websockets Headers: case-insensitive, read with .get
        self.frames: queue.Queue = queue.Queue()

        def process_request(conn, request):
            self.headers = request.headers
            if reject is not None:
                return conn.respond(reject, "rejected\n")
            return None

        def handler(ws):
            try:
                script(ws, self)
            except ConnectionClosed:
                pass  # the client closed at the end of the test

        self.server = serve(handler, "127.0.0.1", 0, process_request=process_request)
        self.port = self.server.socket.getsockname()[1]
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}"

    def next_frame(self, ws) -> tuple[dict, bytes]:
        header, payload = dpc.decode_binary(ws.recv())
        self.frames.put(header)
        return header, payload

    def stop(self):
        self.server.shutdown()


class Recorder:
    """Collects the callbacks."""

    def __init__(self):
        self.results: queue.Queue = queue.Queue()
        self.errors: queue.Queue = queue.Queue()
        self.policies: queue.Queue = queue.Queue()
        self.closed: queue.Queue = queue.Queue()

    def client(self, server: FakeServer) -> dpc.DataPlaneClient:
        return dpc.DataPlaneClient(
            server.url,
            "web_96",
            "S1",
            on_result=self.results.put,
            on_error=self.errors.put,
            on_policy=self.policies.put,
            on_closed=self.closed.put,
        )


def pending(seq: int, split: int = 4) -> dpc.Pending:
    return dpc.Pending(
        seq,
        split,
        2,
        activation=None,
        captured_at="2026-10-21T10:00:00Z",
        deadline_ms=45.0,
    )


@pytest.fixture
def fake():
    servers = []

    def start(script, reject=None):
        s = FakeServer(script, reject)
        servers.append(s)
        return s

    yield start
    for s in servers:
        s.stop()


# --- encoding -------------------------------------------------------------------


def test_binary_roundtrip():
    msg = dpc.encode_binary({"type": "frame", "sequence_id": 7}, b"\x01\x02")
    assert msg[:4] == len(b'{"type":"frame","sequence_id":7}').to_bytes(4, "little")
    assert dpc.decode_binary(msg) == ({"type": "frame", "sequence_id": 7}, b"\x01\x02")


def test_decode_binary_errors():
    with pytest.raises(ValueError):
        dpc.decode_binary(b"\x01\x00")
    with pytest.raises(ValueError):
        dpc.decode_binary((100).to_bytes(4, "little") + b"{}")


def test_stream_url():
    assert dpc.stream_url("http://127.0.0.1:8080") == "ws://127.0.0.1:8080/api/v1/stream"
    assert dpc.stream_url("https://edge.lab:443/") == "wss://edge.lab:443/api/v1/stream"


# --- stream ---------------------------------------------------------------------


def test_open_reads_hello_and_sends_ids(fake):
    def script(ws, srv):
        ws.send(hello())
        ws.recv()  # until the client closes

    server = fake(script)
    rec = Recorder()
    client = rec.client(server)
    h = client.open()
    assert h.max_inflight == 3 and h.input_bytes == SIZES and h.policy.split_point == 4
    assert server.headers.get("X-SCIoT-Device") == "web_96"
    assert server.headers.get("X-SCIoT-Session") == "S1"
    client.close()


def test_results_out_of_order(fake):
    def script(ws, srv):
        ws.send(hello())
        srv.next_frame(ws)
        srv.next_frame(ws)
        ws.send(result(2))
        ws.send(result(1))
        ws.recv()

    server = fake(script)
    rec = Recorder()
    client = rec.client(server)
    client.open()
    assert client.send(pending(1), b"\0" * 8)
    assert client.send(pending(2), b"\0" * 8)

    got = [rec.results.get(timeout=5), rec.results.get(timeout=5)]
    assert [r.pending.sequence_id for r in got] == [2, 1]
    assert all(r.rtt_ms >= 0 and r.result["timing"]["server_ms"] == 1.5 for r in got)
    assert client.inflight == 0

    first = server.frames.get(timeout=5)
    assert first["type"] == "frame" and first["split_point"] == 4 and first["encoding"] == "float32"
    assert first["deadline_ms"] == 45.0 and first["want_raw"] is False
    client.close()


def test_window_full_returns_false(fake):
    def script(ws, srv):
        ws.send(hello(max_inflight=1))
        srv.next_frame(ws)
        ws.recv()

    rec = Recorder()
    client = rec.client(fake(script))
    client.open()
    assert client.send(pending(1), b"\0" * 8)
    assert not client.send(pending(2), b"\0" * 8), "finestra piena: il frame va LOCAL"
    client.close()


def test_policy_window_below_and_above_the_hello(fake):
    def script(ws, srv):
        ws.send(hello(max_inflight=2))
        srv.next_frame(ws)
        srv.next_frame(ws)
        ws.recv()

    rec = Recorder()
    client = rec.client(fake(script))
    client.open()
    assert client.send(pending(1), b"\0" * 8, window=1)
    assert not client.send(pending(2), b"\0" * 8, window=1), "window 1 of the policy"
    # A window above the hello's maximum is capped by it.
    assert client.send(pending(3), b"\0" * 8, window=5)
    assert not client.send(pending(4), b"\0" * 8, window=5)
    client.close()


def test_error_frees_the_window(fake):
    def script(ws, srv):
        ws.send(hello(max_inflight=1))
        header, _ = srv.next_frame(ws)
        ws.send(
            json.dumps(
                {
                    "type": "error",
                    "sequence_id": header["sequence_id"],
                    "code": "server_busy",
                    "message": "predicted wait 61 ms",
                }
            )
        )
        srv.next_frame(ws)
        ws.send(result(2))
        ws.recv()

    rec = Recorder()
    client = rec.client(fake(script))
    client.open()
    assert client.send(pending(1), b"\0" * 8)
    err = rec.errors.get(timeout=5)
    assert err.code == "server_busy" and err.pending.sequence_id == 1
    assert client.send(pending(2), b"\0" * 8), "l'errore libera il posto nella finestra"
    assert rec.results.get(timeout=5).pending.sequence_id == 2
    client.close()


def test_wait_for_slot_wakes_on_the_answer(fake):
    release = threading.Event()

    def script(ws, srv):
        ws.send(hello(max_inflight=1))
        header, _ = srv.next_frame(ws)
        release.wait(5)
        ws.send(result(header["sequence_id"]))
        ws.recv()

    rec = Recorder()
    client = rec.client(fake(script))
    client.open()
    assert client.send(pending(1), b"\0" * 8)
    assert client.slot_eta_s(None, 1000.0) == pytest.approx(1.0, abs=0.2), "full: ~1 s left"
    assert not client.wait_for_slot(None, 0.05), "nothing answered: timeout"
    threading.Timer(0.05, release.set).start()
    start = time.monotonic()
    assert client.wait_for_slot(None, 5.0), "the answer frees the place"
    assert time.monotonic() - start < 2, "woken by the answer, not by the timeout"
    assert client.slot_eta_s(None, 1000.0) is None, "not full any more"
    client.close()
    assert not client.wait_for_slot(None, 1.0), "closed: no place to wait for"


def test_policy_push(fake):
    def script(ws, srv):
        ws.send(hello())
        ws.send(
            json.dumps(
                {
                    "type": "policy",
                    "policy": {
                        "mode": "LOCAL",
                        "split_point": None,
                        "policy_version": 3,
                    },
                }
            )
        )
        ws.recv()

    rec = Recorder()
    client = rec.client(fake(script))
    client.open()
    assert rec.policies.get(timeout=5) == Policy("LOCAL", None, 3)
    client.close()


def test_probe_request_is_answered(fake):
    got: queue.Queue = queue.Queue()

    def script(ws, srv):
        ws.send(hello())
        ws.send(json.dumps({"type": "probe_request", "probe_id": 7, "bytes": 4096}))
        got.put(dpc.decode_binary(ws.recv()))
        ws.recv()

    rec = Recorder()
    client = rec.client(fake(script))
    client.open()
    header, payload = got.get(timeout=5)
    assert header == {"type": "probe", "probe_id": 7}
    assert payload == bytes(4096)
    client.close()


def test_close_fails_frames_in_flight(fake):
    def script(ws, srv):
        ws.send(hello())
        srv.next_frame(ws)
        ws.close(code=dpc.CLOSE_SESSION_EXPIRED, reason="session expired")

    rec = Recorder()
    client = rec.client(fake(script))
    client.open()
    assert client.send(pending(1), b"\0" * 8)

    err = rec.errors.get(timeout=5)
    assert err.code == dpc.STREAM_CLOSED and err.pending.sequence_id == 1
    assert rec.closed.get(timeout=5) == dpc.CLOSE_SESSION_EXPIRED
    assert client.closed
    assert not client.send(pending(2), b"\0" * 8), "stream chiuso: il frame va LOCAL"


def test_wrong_payload_size_is_a_bug(fake):
    def script(ws, srv):
        ws.send(hello())
        ws.recv()

    rec = Recorder()
    client = rec.client(fake(script))
    client.open()
    with pytest.raises(ValueError, match="expects 8"):
        client.send(pending(1), b"\0" * 7)
    with pytest.raises(ValueError, match="no data plane"):
        client.send(pending(1, split=2), b"\0" * 8)
    client.close()


def test_rejected_handshake(fake):
    server = fake(lambda ws, srv: None, reject=409)
    with pytest.raises(InvalidStatus) as exc:
        Recorder().client(server).open()
    assert exc.value.response.status_code == 409


def test_send_while_receiving(fake):
    # websockets.sync: send() from this thread while the receiver thread is
    # blocked in recv(). 200 frames, each answered as soon as it arrives.
    n = 200

    def script(ws, srv):
        ws.send(hello(max_inflight=n))
        for _ in range(n):
            header, _ = dpc.decode_binary(ws.recv())
            ws.send(result(header["sequence_id"]))
        ws.recv()

    rec = Recorder()
    client = rec.client(fake(script))
    client.open()
    for seq in range(1, n + 1):
        assert client.send(pending(seq), b"\0" * 8)
    seen = {rec.results.get(timeout=5).pending.sequence_id for _ in range(n)}
    assert seen == set(range(1, n + 1))
    client.close()
    assert rec.errors.empty()
