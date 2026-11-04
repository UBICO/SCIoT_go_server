"""The pure parts of scripts/bench_multi.py: no servers, no TensorFlow."""

import pytest

from scripts.bench_multi import (
    device_metrics,
    format_table,
    max_pending,
    policy_label,
    policy_timeline,
)


def test_policy_label():
    assert policy_label({"mode": "LOCAL", "split_point": None}) == "LOCAL"
    assert policy_label({"mode": "SPLIT", "split_point": 6, "max_inflight": 2}) == "SPLIT 6/2"
    assert policy_label({"mode": "SPLIT", "split_point": -1}) == "SPLIT -1/max"


def test_device_metrics_skip_the_warmup():
    frames = [
        {"t_s": str(t), "latency_ms": str(lat), "outcome": out}
        for t, lat, out in [
            (1, 999, "local"),
            (11, 10, "result"),
            (12, 20, "result"),
            (13, 30, "local"),
        ]
    ]
    m = device_metrics(frames, warmup_s=10, end_s=14)
    assert m["frames"] == 3 and m["fps"] == pytest.approx(0.75)
    assert m["result_fraction"] == pytest.approx(2 / 3)
    assert m["latency_ms_p50"] == pytest.approx(20)


def test_timeline_and_pending():
    events = [
        {"kind": "start", "t_ms": 0},
        {
            "kind": "policy",
            "t_ms": 1500,
            "device_id": "a",
            "reason": "admin",
            "policy": {"mode": "LOCAL", "split_point": None, "policy_version": 2},
        },
        {"kind": "decision", "t_ms": 12000, "pending_erlangs": 0.4},
        {
            "kind": "policy",
            "t_ms": 12000,
            "device_id": "a",
            "reason": "latency",
            "policy": {"mode": "SPLIT", "split_point": 6, "policy_version": 3, "max_inflight": 2},
        },
        {"kind": "decision", "t_ms": 12500, "pending_erlangs": 1.3},
    ]
    timeline = policy_timeline(events)
    assert [(c["t_s"], c["policy"], c["reason"]) for c in timeline] == [
        (1.5, "LOCAL", "admin"),
        (12.0, "SPLIT 6/2", "latency"),
    ]
    assert max_pending(events) == pytest.approx(1.3)
    assert max_pending([]) == 0.0

    table = format_table(
        {"a": {"fps": 50, "latency_ms_p50": 40, "latency_ms_p95": None, "result_fraction": 1}},
        timeline,
    )
    # The admin check of the benchmark is not a change of the algorithm.
    assert "| a | 50.0 | 40.0 | — | 100% | 1 | SPLIT 6/2 |" in table
    assert "admin" not in table
