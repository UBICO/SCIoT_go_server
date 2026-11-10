import math
import os
import threading

import pytest
import requests

from sciot_client import control_plane_client as cpc
from sciot_client import segments

MODEL_HASH = "2b319af7002697e06bf876905d72d6a5"

OK_BODY = {
    "session_id": "S1",
    "model": {
        "model_key": "yolo_small_320",
        "num_layers": 8,
        "last_offloading_layer": 7,
        "input": {"shape": [1, 320, 320, 3], "dtype": "float32"},
        "postprocess": {
            "decoder": "yolo_pro",
            "labels": ["bicycle", "stop sign"],
            "score_threshold": 0.5,
            "iou_threshold": 0.2,
            "max_detections": 10,
        },
    },
    "policy": {"mode": "LOCAL", "split_point": None, "policy_version": 1},
    "heartbeat_interval_ms": 1000,
}


class FakeResponse:
    def __init__(self, status_code, body=None, text=None):
        self.status_code = status_code
        self._body = body
        self.text = text if text is not None else ""

    def json(self):
        if self._body is None:
            raise ValueError("no JSON body")
        return self._body


class FakeHTTP:
    """Records the last POST and returns a canned response (or raises)."""

    def __init__(self, response=None, exc=None):
        self.response = response
        self.exc = exc
        self.calls = []

    def post(self, url, json=None, timeout=None):
        self.calls.append({"url": url, "json": json, "timeout": timeout})
        if self.exc is not None:
            raise self.exc
        return self.response


def make_client(http):
    return cpc.ControlPlaneClient("http://server:8080/", "web_96", http=http)


# --- parsing ---------------------------------------------------------------


def test_session_from_dict():
    s = cpc.Session.from_dict(OK_BODY)
    assert s.session_id == "S1"
    assert s.model == cpc.ModelInfo(
        "yolo_small_320",
        8,
        7,
        cpc.TensorSpec((1, 320, 320, 3), "float32"),
        cpc.Postprocess("yolo_pro", ("bicycle", "stop sign"), 0.5, 10, 0.2),
    )
    assert s.policy == cpc.Policy("LOCAL", None, 1)
    assert s.heartbeat_interval_ms == 1000


def test_policy_split_ok():
    p = cpc.Policy.from_dict({"mode": "SPLIT", "split_point": 5, "policy_version": 2})
    assert p == cpc.Policy("SPLIT", 5, 2)
    p = cpc.Policy.from_dict(
        {"mode": "SPLIT", "split_point": 5, "policy_version": 3, "max_inflight": 2}
    )
    assert p == cpc.Policy("SPLIT", 5, 3, 2)


@pytest.mark.parametrize(
    "policy",
    [
        {"mode": "REMOTE", "split_point": None, "policy_version": 1},
        {"mode": "LOCAL", "split_point": 3, "policy_version": 1},
        {"mode": "SPLIT", "split_point": None, "policy_version": 1},
        {"mode": "SPLIT", "split_point": "5", "policy_version": 1},
        {"mode": "SPLIT", "split_point": 5.0, "policy_version": 1},
        {"mode": "SPLIT", "split_point": True, "policy_version": 1},
        {"mode": "SPLIT", "split_point": 5, "policy_version": 1, "max_inflight": 0},
        {"mode": "SPLIT", "split_point": 5, "policy_version": 1, "max_inflight": True},
        {"mode": "SPLIT", "split_point": 5, "policy_version": 1, "max_inflight": 2.0},
        {"mode": "LOCAL", "split_point": None, "policy_version": 1, "max_inflight": 2},
    ],
)
def test_policy_invalid(policy):
    with pytest.raises(ValueError):
        cpc.Policy.from_dict(policy)


def test_model_missing_key():
    with pytest.raises(KeyError):
        cpc.ModelInfo.from_dict({"model_key": "x", "num_layers": 1})
    # The old field is gone: a model without "input" is an error, not a default.
    old = {
        "model_key": "x",
        "num_layers": 1,
        "last_offloading_layer": 0,
        "input_size_bytes": 1,
    }
    with pytest.raises(KeyError):
        cpc.ModelInfo.from_dict(old)


def test_postprocess_without_nms():
    p = cpc.Postprocess.from_dict(
        {
            "decoder": "fomo",
            "labels": ["a"],
            "score_threshold": 0.8,
            "max_detections": 10,
        }
    )
    assert p.iou_threshold is None and p.labels == ("a",)


def test_session_data_plane():
    assert cpc.Session.from_dict({**OK_BODY, "data_plane": True}).data_plane is True


def test_session_empty_id():
    with pytest.raises(ValueError):
        cpc.Session.from_dict({**OK_BODY, "session_id": ""})


# --- register --------------------------------------------------------------


def test_register_ok_sends_contract_payload():
    http = FakeHTTP(FakeResponse(200, OK_BODY))
    session = make_client(http).register(MODEL_HASH, probe=True)

    assert session.session_id == "S1"
    assert session.data_plane is False  # absent in OK_BODY: an older server
    (call,) = http.calls
    assert call["url"] == "http://server:8080/api/v1/register"
    assert call["timeout"] == 5.0
    assert call["json"] == {
        "protocol_version": 1,
        "device_id": "web_96",
        "device_type": "python",
        "model_hash": MODEL_HASH,
        "capabilities": {"probe": True, "wait_for_window": False},
    }


@pytest.mark.parametrize(
    "status, code, exc_class",
    [
        (404, "model_not_found", cpc.ModelNotFound),
        (400, "unsupported_protocol", cpc.UnsupportedProtocol),
        (400, "invalid_request", cpc.ProtocolError),
    ],
)
def test_register_error_mapping(status, code, exc_class):
    body = {"error": {"code": code, "message": "boom"}}
    http = FakeHTTP(FakeResponse(status, body))

    with pytest.raises(exc_class) as info:
        make_client(http).register(MODEL_HASH)

    assert type(info.value) is exc_class
    assert info.value.status == status
    assert info.value.code == code
    assert info.value.message == "boom"


def test_register_non_json_error():
    http = FakeHTTP(FakeResponse(502, None, text="<html>Bad Gateway</html>"))
    with pytest.raises(cpc.ProtocolError) as info:
        make_client(http).register(MODEL_HASH)
    assert info.value.status == 502
    assert info.value.code == "unexpected_response"
    assert "Bad Gateway" in info.value.message


def test_register_unreachable():
    http = FakeHTTP(exc=requests.ConnectionError("refused"))
    with pytest.raises(cpc.ServerUnreachable) as info:
        make_client(http).register(MODEL_HASH)
    assert isinstance(info.value.__cause__, requests.ConnectionError)


def test_register_malformed_success_body():
    http = FakeHTTP(FakeResponse(200, {**OK_BODY, "policy": {"mode": "REMOTE"}}))
    with pytest.raises((ValueError, KeyError)):
        make_client(http).register(MODEL_HASH)


# --- inference window --------------------------------------------------------


def test_window_empty():
    assert cpc.InferenceWindow().snapshot_and_reset() == cpc.WindowStats(0, None, None)


def test_window_stats_and_reset():
    w = cpc.InferenceWindow()
    for ms in (10.0, 30.0, 20.0):
        w.record(ms)

    assert w.snapshot_and_reset() == cpc.WindowStats(3, 20.0, 30.0)
    # The snapshot starts a new, empty window.
    assert w.snapshot_and_reset() == cpc.WindowStats(0, None, None)


def test_window_max_after_reset():
    # After a reset the max must start over, not remember the old window.
    w = cpc.InferenceWindow()
    w.record(100.0)
    w.snapshot_and_reset()
    w.record(5.0)
    assert w.snapshot_and_reset().max_ms == 5.0


@pytest.mark.parametrize("bad", [-1.0, float("nan"), float("inf")])
def test_window_rejects_invalid_times(bad):
    # NaN would pass a "< 0" check (every comparison with NaN is False) and
    # be serialized as the non-standard JSON token NaN, which the server rejects.
    with pytest.raises(ValueError):
        cpc.InferenceWindow().record(bad)


def test_window_concurrent_record():
    w = cpc.InferenceWindow()
    snapshots = []

    def loop():
        for _ in range(2000):
            w.record(1.0)

    def heartbeats():
        for _ in range(50):
            snapshots.append(w.snapshot_and_reset())

    threads = [threading.Thread(target=loop) for _ in range(4)]
    threads.append(threading.Thread(target=heartbeats))
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    snapshots.append(w.snapshot_and_reset())

    # No inference lost or counted twice across all windows.
    assert sum(s.count for s in snapshots) == 4 * 2000


# --- telemetry serialization ---------------------------------------------------


def test_telemetry_empty_window():
    t = cpc.Telemetry(cpc.WindowStats(0, None, None))
    assert t.to_dict() == {"inference_count": 0}


def test_telemetry_full():
    t = cpc.Telemetry(
        cpc.WindowStats(47, 21.3, 38.9),
        cpu_load=0.42,
        rtt_ms=3.1,
        memory_available_bytes=812345344,
    )
    assert t.to_dict() == {
        "inference_count": 47,
        "inference_time_ms": 21.3,
        "inference_time_max_ms": 38.9,
        "cpu_load": 0.42,
        "rtt_ms": 3.1,
        "memory_available_bytes": 812345344,
    }


def test_telemetry_keeps_zero_values():
    # 0.0 is a value, not "absent": it must be sent.
    t = cpc.Telemetry(cpc.WindowStats(0, None, None), cpu_load=0.0, rtt_ms=0.0)
    assert t.to_dict() == {"inference_count": 0, "cpu_load": 0.0, "rtt_ms": 0.0}


# --- heartbeat -------------------------------------------------------------


def make_session():
    return cpc.Session.from_dict(OK_BODY)


def test_heartbeat_ok_sends_contract_payload():
    split = {"mode": "SPLIT", "split_point": 5, "policy_version": 2}
    http = FakeHTTP(FakeResponse(200, {"policy": split}))
    session = make_session()
    telemetry = cpc.Telemetry(cpc.WindowStats(3, 20.0, 30.0), cpu_load=0.5)

    policy = make_client(http).heartbeat(session, session.policy, telemetry)

    assert policy == cpc.Policy("SPLIT", 5, 2)
    (call,) = http.calls
    assert call["url"] == "http://server:8080/api/v1/heartbeat"
    assert call["json"] == {
        "device_id": "web_96",
        "session_id": "S1",
        "policy_version": 1,
        "mode": "LOCAL",
        "telemetry": {
            "inference_count": 3,
            "inference_time_ms": 20.0,
            "inference_time_max_ms": 30.0,
            "cpu_load": 0.5,
        },
    }


def test_heartbeat_session_expired():
    body = {"error": {"code": "session_expired", "message": "session expired"}}
    http = FakeHTTP(FakeResponse(409, body))
    session = make_session()
    empty = cpc.Telemetry(cpc.WindowStats(0, None, None))

    with pytest.raises(cpc.SessionExpired) as info:
        make_client(http).heartbeat(session, session.policy, empty)
    # Still a ProtocolError: generic handlers keep working.
    assert isinstance(info.value, cpc.ProtocolError)
    assert info.value.status == 409


def test_heartbeat_unreachable():
    http = FakeHTTP(exc=requests.Timeout("slow"))
    session = make_session()
    empty = cpc.Telemetry(cpc.WindowStats(0, None, None))
    with pytest.raises(cpc.ServerUnreachable):
        make_client(http).heartbeat(session, session.policy, empty)


def test_heartbeat_invalid_policy():
    bad = {"policy": {"mode": "LOCAL", "split_point": 3, "policy_version": 2}}
    http = FakeHTTP(FakeResponse(200, bad))
    session = make_session()
    empty = cpc.Telemetry(cpc.WindowStats(0, None, None))
    with pytest.raises(ValueError):
        make_client(http).heartbeat(session, session.policy, empty)


# --- against a running Go server --------------------------------------------


@pytest.mark.external_server
def test_pairing_with_go_server():
    """Run with the server's model directory:

    SCIOT_GO_SERVER_URL=http://127.0.0.1:8080 SCIOT_MODEL_DIR=~/sciot-models/yolo_small_320 \
        uv run --project client pytest -m external_server
    """
    url, model_dir = os.environ.get("SCIOT_GO_SERVER_URL"), os.environ.get("SCIOT_MODEL_DIR")
    if not url or not model_dir:
        pytest.skip("SCIOT_GO_SERVER_URL or SCIOT_MODEL_DIR not set")
    model_hash = segments.model_hash(os.path.expanduser(model_dir))

    session = cpc.ControlPlaneClient(url, "pytest-web_96").register(model_hash)
    assert session.policy.mode == cpc.MODE_LOCAL
    assert session.model.model_key

    window = cpc.InferenceWindow()
    for ms in (20.0, 25.0):
        window.record(ms)
    telemetry = cpc.Telemetry(window.snapshot_and_reset(), cpu_load=0.3)
    client = cpc.ControlPlaneClient(url, "pytest-web_96")
    policy = client.heartbeat(client.register(model_hash), session.policy, telemetry)
    assert policy.mode == cpc.MODE_LOCAL


# --- per-segment times ------------------------------------------------------------


def test_window_segments_and_reset():
    w = cpc.InferenceWindow()
    w.record_segments(0, [0.001, 0.003])  # prefix 0..1
    w.record_segments(1, [0.005, 0.002])  # completion 1..2 of a refused frame
    stats = w.snapshot_and_reset()
    assert stats.segments == {
        0: pytest.approx((1.0, 1)),
        1: pytest.approx((8.0, 2)),
        2: pytest.approx((2.0, 1)),
    }
    assert w.snapshot_and_reset().segments == {}


@pytest.mark.parametrize("bad", [-0.001, math.nan, math.inf])
def test_window_rejects_invalid_segment_times(bad):
    with pytest.raises(ValueError):
        cpc.InferenceWindow().record_segments(0, [0.001, bad])


def test_telemetry_segments():
    w = cpc.InferenceWindow()
    w.record_segments(0, [0.001, 0.003])
    w.record_segments(0, [0.003, 0.005])
    data = cpc.Telemetry(w.snapshot_and_reset(), num_segments=3).to_dict()
    assert data["segment_ms"] == [pytest.approx(2.0), pytest.approx(4.0), None]
    assert data["segment_count"] == [2, 2, 0]


def test_telemetry_segments_omitted_without_model_or_runs():
    w = cpc.InferenceWindow()
    assert "segment_ms" not in cpc.Telemetry(w.snapshot_and_reset(), num_segments=3).to_dict()
    w.record_segments(0, [0.001])
    assert "segment_ms" not in cpc.Telemetry(w.snapshot_and_reset()).to_dict()


def test_telemetry_segment_outside_the_model_is_a_bug():
    w = cpc.InferenceWindow()
    w.record_segments(2, [0.001])
    with pytest.raises(ValueError):
        cpc.Telemetry(w.snapshot_and_reset(), num_segments=2).to_dict()
