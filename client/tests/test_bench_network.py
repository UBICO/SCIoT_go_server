"""The pure parts of scripts/bench_network.py, and the proxy's upload limit."""

from __future__ import annotations

import socket
import threading
import time
from datetime import UTC, datetime

import pytest

from scripts.bench_common import CountingProxy
from scripts.bench_network import (
    format_table,
    in_window,
    network_metrics,
    parse_args,
    parse_ts,
)


def test_parse_ts_accepts_nanoseconds():
    got = parse_ts("2026-10-23T10:00:01.123456789Z")
    assert got == datetime(2026, 10, 23, 10, 0, 1, 123456, tzinfo=UTC)
    assert parse_ts("2026-10-23T10:00:01Z").second == 1


def heartbeat(ts, bw, valid, discarded, latency=1.5, not_limiting=False):
    return {
        "kind": "heartbeat",
        "ts": ts,
        "device_id": "d",
        "network": {
            "bandwidth_bps": bw,
            "latency_ms": latency,
            "not_limiting": not_limiting,
            "valid_samples": valid,
            "discarded_samples": discarded,
        },
    }


def frame(ts, nbytes, receive_ms, valid=True):
    return {
        "kind": "frame",
        "ts": ts,
        "device_id": "d",
        "request_bytes": nbytes,
        "receive_ms": receive_ms,
        "receive_valid": valid,
    }


def test_in_window_filters_kind_device_and_time():
    events = [
        heartbeat("2026-10-23T10:00:00Z", 1, 0, 0),
        heartbeat("2026-10-23T10:00:05Z", 1, 0, 0),
        {**heartbeat("2026-10-23T10:00:05Z", 1, 0, 0), "device_id": "other"},
        frame("2026-10-23T10:00:05Z", 1, 1),
        heartbeat("2026-10-23T10:00:10Z", 1, 0, 0),  # end is excluded
    ]
    start = datetime(2026, 10, 23, 10, 0, 1, tzinfo=UTC)
    end = datetime(2026, 10, 23, 10, 0, 10, tzinfo=UTC)
    got = in_window(events, "heartbeat", "d", start, end)
    assert [e["ts"] for e in got] == ["2026-10-23T10:00:05Z"]


def test_network_metrics():
    hbs = [
        heartbeat("t1", 1.0e6, 10, 2),
        heartbeat("t2", 1.25e6, 15, 2, latency=2.5),
        heartbeat("t3", 1.5e6, 20, 3, latency=0.5),
    ]
    frames = [
        frame("t", 125_000, 100.0),  # 1.25e6 B/s = 10 Mbit/s
        frame("t", 125_000, 50.0),  # 20 Mbit/s
        frame("t", 125_000, 1.0, valid=False),  # from the buffer: ignored
    ]
    m = network_metrics(hbs, frames)
    assert m["estimate_mbps"] == pytest.approx(10.0)  # median 1.25e6 B/s
    assert m["frame_valid_mbps"] == pytest.approx(15.0)
    assert (m["valid_samples"], m["discarded_samples"]) == (10, 1)
    assert m["latency_ms"] == 1.5 and m["not_limiting"] is False
    assert m["receive_ms_p50"] == 50.0 and m["frames"] == 3


def test_network_metrics_counts_probes():
    probe = {
        "kind": "probe",
        "bytes": 125_000,
        "receive_ms": 100.0,
        "receive_valid": True,
    }
    m = network_metrics([], [probe])
    assert m["frame_valid_mbps"] == pytest.approx(10.0) and m["receive_ms_p50"] == 100.0


def test_network_metrics_without_data():
    m = network_metrics([{"kind": "heartbeat", "ts": "t"}], [])
    assert m["estimate_mbps"] is None and m["valid_samples"] is None
    assert m["not_limiting"] is None and m["latency_ms"] is None


def test_format_table():
    base = {
        "carried_mbps": 9.6,
        "estimate_mbps": 9.5,
        "frame_valid_mbps": 9.7,
        "valid_samples": 20,
        "discarded_samples": 1,
        "not_limiting": False,
        "latency_ms": 0.12,
        "receive_ms_p50": 980.0,
    }
    table = format_table(
        [
            {"limit_mbps": 10.0, **base},
            {
                "limit_mbps": 0.0,
                **base,
                "estimate_mbps": None,
                "valid_samples": None,
                "not_limiting": True,
            },
        ]
    )
    lines = table.splitlines()
    assert "| 10.0 | 9.6 | 9.5 | 0.95 | 9.7 | 20 / 1 | False |" in lines[2]
    assert lines[3].startswith("| none | 9.6 | — | — |") and "| True |" in lines[3]


def test_parse_args_limits(tmp_path):
    (tmp_path / "manifest.json").write_text("{}")
    args = parse_args(["--model-dir", str(tmp_path), "--go-bin", "x", "--limits-mbps", "5,0"])
    assert args.limits == [5.0, 0.0] and args.split == "-1"
    with pytest.raises(SystemExit):
        parse_args(["--model-dir", str(tmp_path), "--go-bin", "x", "--limits-mbps", "a"])


def _sink_server() -> tuple[socket.socket, list[int]]:
    """A TCP server that reads and counts everything it receives."""
    srv = socket.create_server(("127.0.0.1", 0))
    received = [0]

    def serve():
        conn, _ = srv.accept()
        with conn:
            while data := conn.recv(65536):
                received[0] += len(data)

    threading.Thread(target=serve, daemon=True).start()
    return srv, received


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.mark.parametrize("limit", [None, 2e6])
def test_proxy_upload_limit(limit):
    srv, received = _sink_server()
    proxy = CountingProxy(_free_port(), "127.0.0.1", srv.getsockname()[1]).start()
    try:
        proxy.set_up_limit(limit)
        payload = b"x" * 500_000
        start = time.monotonic()
        with socket.create_connection(("127.0.0.1", proxy.listen_port)) as c:
            c.sendall(payload)
            deadline = time.monotonic() + 5
            while received[0] < len(payload) and time.monotonic() < deadline:
                time.sleep(0.005)
        elapsed = time.monotonic() - start
        assert received[0] == len(payload) and proxy.counts()[0] == len(payload)
        if limit is None:
            assert elapsed < 0.2
        else:
            # 500 KB at 2 MB/s: 0.25 s (the first chunk goes out at once).
            assert 0.2 < elapsed < 0.4
    finally:
        proxy.stop()
        srv.close()
