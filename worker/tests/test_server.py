import threading
import time

import grpc
import numpy as np
import pytest
from conftest import NUM_SEGMENTS, expected_output

from sciot_worker.models import model_hash
from sciot_worker.server import ModelConfig, PartitionWorker, build_server
from sciot_worker.v1 import worker_pb2, worker_pb2_grpc


@pytest.fixture
def make_worker(model_dir):
    """Start an in-process worker on a free port; return (worker, stub)."""
    servers = []

    def make(slots=2, threads=1):
        worker = PartitionWorker([ModelConfig("tiny", model_dir)], slots, threads)
        server, port = build_server(worker, "127.0.0.1:0")
        server.start()
        servers.append(server)
        channel = grpc.insecure_channel(f"127.0.0.1:{port}")
        return worker, worker_pb2_grpc.PartitionWorkerStub(channel)

    yield make
    for server in servers:
        server.stop(grace=None)


def tensor(width=4):
    return np.arange(width, dtype=np.float32).reshape(1, width)


def test_describe(make_worker, model_dir):
    _, stub = make_worker(slots=3, threads=2)
    resp = stub.Describe(worker_pb2.DescribeRequest())

    assert resp.slots == 3 and resp.threads == 2
    (model,) = resp.models
    assert model.model_key == "tiny"
    assert model.model_hash == model_hash(model_dir)
    assert [s.index for s in model.segments] == list(range(NUM_SEGMENTS))
    first = model.segments[0]
    assert list(first.input_shape) == [1, 6] and first.input_dtype == "float32"
    assert first.input_bytes == 24 and first.output_bytes == 16


def test_run_from(make_worker):
    _, stub = make_worker()
    x = tensor()
    resp = stub.RunFrom(
        worker_pb2.RunRequest(model_key="tiny", split_point=4, tensor=x.tobytes(), want_raw=True)
    )

    got = np.frombuffer(resp.output, dtype=np.float32).reshape(1, 4)
    np.testing.assert_allclose(got, expected_output(x, 4))
    assert len(resp.layer_ms) == NUM_SEGMENTS - 5
    assert resp.queue_wait_ms >= 0


def test_unknown_model(make_worker):
    _, stub = make_worker()
    with pytest.raises(grpc.RpcError) as info:
        stub.RunFrom(worker_pb2.RunRequest(model_key="nope", split_point=0, tensor=b""))
    assert info.value.code() == grpc.StatusCode.NOT_FOUND


def test_invalid_argument_returns_the_slot(make_worker):
    worker, stub = make_worker(slots=1)
    for _ in range(3):  # with a lost slot the second call would hang
        with pytest.raises(grpc.RpcError) as info:
            stub.RunFrom(
                worker_pb2.RunRequest(model_key="tiny", split_point=0, tensor=b"\0" * 15),
                timeout=2,
            )
        assert info.value.code() == grpc.StatusCode.INVALID_ARGUMENT
    assert worker.free.qsize() == 1
    stub.RunFrom(worker_pb2.RunRequest(model_key="tiny", split_point=0, tensor=tensor().tobytes()))


def test_deadline_while_waiting_for_a_slot(make_worker):
    worker, stub = make_worker(slots=1)
    release = threading.Event()
    original = worker.slots[0].models["tiny"].run_from

    def slow_run_from(split_point, data):
        release.wait(5)
        return original(split_point, data)

    worker.slots[0].models["tiny"].run_from = slow_run_from

    request = worker_pb2.RunRequest(model_key="tiny", split_point=0, tensor=tensor().tobytes())
    busy = threading.Thread(target=lambda: stub.RunFrom(request, timeout=10))
    busy.start()
    time.sleep(0.2)  # the only slot is now taken

    with pytest.raises(grpc.RpcError) as info:
        stub.RunFrom(request, timeout=0.3)
    assert info.value.code() == grpc.StatusCode.DEADLINE_EXCEEDED

    release.set()
    busy.join(5)
    assert worker.free.qsize() == 1


def test_queue_wait_is_measured(make_worker):
    worker, stub = make_worker(slots=1)
    original = worker.slots[0].models["tiny"].run_from

    def slow_run_from(split_point, data):
        time.sleep(0.2)
        return original(split_point, data)

    worker.slots[0].models["tiny"].run_from = slow_run_from
    request = worker_pb2.RunRequest(model_key="tiny", split_point=0, tensor=tensor().tobytes())

    results = []
    threads = [
        threading.Thread(target=lambda: results.append(stub.RunFrom(request))) for _ in range(2)
    ]
    for t in threads:
        t.start()
    for t in threads:
        t.join(5)

    waits = sorted(r.queue_wait_ms for r in results)
    assert waits[0] < 50 and waits[1] > 100  # the second request waited for the first


# --- decoding -------------------------------------------------------------------


def yolo_postprocess(**kw) -> worker_pb2.Postprocess:
    fields = {
        "decoder": "yolo_pro",
        "labels": ["bicycle", "stop sign"],
        "score_threshold": 0.5,
        "iou_threshold": 0.5,
        "max_detections": 200,
    }
    fields.update(kw)
    return worker_pb2.Postprocess(**fields)


def fake_output(worker, rows):
    """Make run_from return `rows` as the model output (the tiny model has 4 outputs)."""
    data = np.asarray(rows, dtype=np.float32).tobytes()
    for slot in worker.slots:
        slot.models["tiny"].run_from = lambda split_point, tensor: (data, [1.0])


def test_run_from_decodes(make_worker):
    worker, stub = make_worker()
    fake_output(worker, [[0, 0, 0.75, 1, 0.875, 0], [0.25, 0, 1, 1, 0.75, 0]])
    resp = stub.RunFrom(
        worker_pb2.RunRequest(
            model_key="tiny",
            split_point=0,
            tensor=tensor().tobytes(),
            postprocess=yolo_postprocess(),
        )
    )

    assert [(d.label, d.score) for d in resp.detections] == [("bicycle", 0.875)]
    d = resp.detections[0]
    assert (d.x, d.y, d.w, d.h) == pytest.approx((0, 0, 0.75, 1))
    assert resp.output == b""  # want_raw false: detections only
    assert resp.decode_ms > 0


def test_run_from_without_postprocess_does_not_decode(make_worker):
    worker, stub = make_worker()
    fake_output(worker, [[0, 0, 1, 1, 0.875, 0]])
    resp = stub.RunFrom(
        worker_pb2.RunRequest(
            model_key="tiny", split_point=0, tensor=tensor().tobytes(), want_raw=True
        )
    )
    assert len(resp.detections) == 0 and resp.decode_ms == 0 and len(resp.output) == 24


def test_run_from_without_iou_has_no_nms(make_worker):
    # proto3 "optional": an absent iou_threshold must not become 0.0 (which
    # would suppress every overlapping box).
    worker, stub = make_worker()
    fake_output(worker, [[0, 0, 1, 1, 0.875, 0], [0, 0, 1, 1, 0.75, 0]])
    pp = yolo_postprocess()
    pp.ClearField("iou_threshold")
    resp = stub.RunFrom(
        worker_pb2.RunRequest(
            model_key="tiny", split_point=0, tensor=tensor().tobytes(), postprocess=pp
        )
    )
    assert len(resp.detections) == 2


def test_decode_error_is_invalid_argument(make_worker):
    worker, stub = make_worker()
    fake_output(worker, [[0, 0, 1, 1, 0.875]])  # 5 columns, 2 labels need 6
    with pytest.raises(grpc.RpcError) as err:
        stub.RunFrom(
            worker_pb2.RunRequest(
                model_key="tiny",
                split_point=0,
                tensor=tensor().tobytes(),
                postprocess=yolo_postprocess(),
            )
        )
    assert err.value.code() == grpc.StatusCode.INVALID_ARGUMENT
    assert "decode" in err.value.details()
