"""The pure parts of scripts/bench_split.py: no servers, no TensorFlow."""

from __future__ import annotations

from pathlib import Path

import pytest

from scripts.bench_split import (
    BENCH_IMAGE,
    LOCAL,
    Phase,
    client_config,
    configurations,
    format_table,
    go_config,
    phase_frames,
    phase_metrics,
    policy_body,
    reference_detections,
    schedule,
    server_metrics,
    summarize_configs,
)


def row(t, version, outcome="result", mode="SPLIT", **kw):
    r = {
        "t_s": str(t),
        "policy_version": str(version),
        "outcome": outcome,
        "mode": mode,
        "latency_ms": "40",
        "dev_ms": "10",
        "srv_ms": "25",
        "net_ms": "1",
        "sent_bytes": "819200",
        "detections": "1",
    }
    r.update({k: str(v) for k, v in kw.items()})
    return r


def test_configurations():
    # yolo-small: 8 segments → LOCAL, -1..6.
    assert configurations(8) == [LOCAL, "-1", "0", "1", "2", "3", "4", "5", "6"]
    assert configurations(8, ["-1", LOCAL]) == ["-1", LOCAL]
    with pytest.raises(ValueError, match="7"):
        configurations(8, ["7"])  # 7 = last: that is LOCAL


def test_schedule_alternates():
    assert schedule(["A", "B", "C"], 3) == [
        (1, "A"),
        (1, "B"),
        (1, "C"),
        (2, "C"),
        (2, "B"),
        (2, "A"),
        (3, "A"),
        (3, "B"),
        (3, "C"),
    ]


def test_policy_body():
    assert policy_body(LOCAL) == {"mode": "LOCAL", "split_point": None}
    assert policy_body("-1") == {"mode": "SPLIT", "split_point": -1}


def test_client_config_is_explicit():
    # Every key set: a locally edited go_client.yaml cannot leak in.
    from sciot_client.go_client_config import KEYS

    cfg = client_config("http://127.0.0.1:1", "bench", Path("/m/yolo"))
    assert set(cfg) == set(KEYS)
    assert cfg["image"] == str(BENCH_IMAGE) and BENCH_IMAGE.is_file()


def test_go_config():
    cfg = go_config(Path("/m/yolo"), Path("/go"), 50190, 2, 4, Path("/rec"), 1000)
    assert cfg["models"] == [{"model_key": "bench_yolo", "tflite_dir": "/m/yolo"}]
    assert cfg["worker"]["command"][-1] == "sciot-worker" and cfg["worker"]["threads"] == 4
    assert cfg["record"] == {"dir": "/rec"}
    # Without a repo the worker is external: only its address.
    assert go_config(Path("/m/y"), None, 1, 1, 1, Path("/r"), 1000)["worker"] == {
        "address": "127.0.0.1:1"
    }


def test_phase_frames_uses_version_and_window():
    rows = [row(9.0, 3), row(10.4, 3), row(14.9, 3), row(15.1, 3), row(12.0, 4)]
    # First frame of v3 at 9.0 s; warm-up 1 s, measure 5 s → window [10, 15).
    frames = phase_frames(rows, Phase(1, "4", 3, first_t_s=9.0), warmup_s=1, measure_s=5)
    assert [r["t_s"] for r in frames] == ["10.4", "14.9"]


def test_phase_metrics():
    frames = [
        row(1, 3),
        row(2, 3),
        row(
            3,
            3,
            outcome="local (window_full)",
            latency_ms=70,
            dev_ms=65,
            srv_ms="",
            net_ms="",
            sent_bytes=0,
        ),
        row(4, 3, detections=2),
    ]
    m = phase_metrics(frames, measure_s=2.0, reference_detections=1)
    assert m["frames"] == 4 and m["fps"] == 2.0
    assert m["result_fraction"] == 0.75
    assert m["server_ms_p50"] == 25.0  # results only
    assert m["sent_bytes_per_frame"] == 819200 * 3 / 4
    assert m["outcomes"] == {"result": 3, "local (window_full)": 1}
    assert m["detections_differ"] == 1


def test_phase_metrics_empty():
    m = phase_metrics([], measure_s=5, reference_detections=None)
    assert m["frames"] == 0 and m["latency_ms_p50"] is None and m["detections_differ"] is None


def test_reference_detections():
    rows = [
        row(1, 2, mode=LOCAL, detections=1),
        row(2, 2, mode=LOCAL, detections=1),
        row(3, 2, mode=LOCAL, detections=2),
        row(4, 3, detections=5),
    ]
    assert reference_detections(rows) == 1
    assert reference_detections([row(1, 3)]) is None


def test_server_metrics():
    record = [
        {
            "kind": "frame",
            "policy_version": 3,
            "outcome": "result",
            "timing": {"queue_wait_ms": 4.0},
            "admission": {"predicted_wait_ms": 1.0},
        },
        {
            "kind": "frame",
            "policy_version": 3,
            "outcome": "result",
            "timing": {"queue_wait_ms": 2.0},
            "admission": {"predicted_wait_ms": 2.0},
        },
        {"kind": "frame", "policy_version": 3, "outcome": "server_busy"},
        {
            "kind": "frame",
            "policy_version": 4,
            "outcome": "result",
            "timing": {"queue_wait_ms": 99.0},
        },
    ]
    m = server_metrics(record, 3)
    assert m["server_frames"] == 3
    assert m["queue_wait_ms_p50"] == 3.0
    assert m["wait_prediction_error_ms_p50"] == 1.5
    assert m["server_outcomes"] == {"result": 2, "server_busy": 1}


def test_summary_and_table():
    def phase(config, fps):
        return {
            "config": config,
            "fps": fps,
            "latency_ms_p50": 40.0,
            "latency_ms_p95": 60.0,
            "device_ms_p50": 10.0,
            "server_ms_p50": None if config == LOCAL else 25.0,
            "network_ms_p50": None,
            "sent_bytes_per_frame": 819200.0,
            "result_fraction": 1.0,
            "queue_wait_ms_p50": None,
            "wait_prediction_error_ms_p50": None,
            "wait_prediction_error_ms_p95": None,
            "up_kib_per_s": 1.0,
            "outcomes": {"result": 10},
            "detections_differ": 0,
        }

    summary = summarize_configs([phase("4", 20.0), phase(LOCAL, 15.0), phase("4", 22.0)])
    assert list(summary) == ["4", LOCAL]
    assert summary["4"]["runs"] == 2 and summary["4"]["fps"]["mean"] == 21.0
    assert summary[LOCAL]["server_ms_p50"] is None
    assert summary["4"]["outcomes"] == {"result": 20}

    table = format_table(summary)
    assert "| split 4 | 21.0 ± 1.4 |" in table
    assert "| LOCAL |" in table and "| 800 ± 0 |" in table and "100 ± 0 %" in table


def test_device_time_of_offloaded_frames_only():
    # Half results (device 0 ms, split -1), half finished locally (65 ms):
    # the device time is that of the offloaded frames, it must not jump.
    frames = [row(i, 3, dev_ms=0) for i in range(3)] + [
        row(i, 3, outcome="local (server_busy)", dev_ms=65, srv_ms="", net_ms="") for i in range(4)
    ]
    assert phase_metrics(frames, 1.0, None)["device_ms_p50"] == 0.0
    local = [
        row(i, 2, outcome="local", mode=LOCAL, dev_ms=63, srv_ms="", net_ms="") for i in range(3)
    ]
    assert phase_metrics(local, 1.0, None)["device_ms_p50"] == 63.0
