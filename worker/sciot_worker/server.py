"""gRPC service: Describe and RunFrom over a pool of slots."""

from __future__ import annotations

import queue
import time
from dataclasses import dataclass, field
from pathlib import Path

import grpc

from sciot_worker.models import LoadedModel, model_hash
from sciot_worker.v1 import worker_pb2, worker_pb2_grpc

# gRPC default is 4 MiB; the largest yolo-small activation is 3.2 MB.
MAX_MESSAGE_BYTES = 64 << 20

# Longest wait for a free slot when the caller set no deadline. The Go server
# always sets one; this only protects against callers that do not.
MAX_SLOT_WAIT_S = 60.0


@dataclass(frozen=True)
class ModelConfig:
    model_key: str
    model_dir: Path


@dataclass
class Slot:
    """A complete set of interpreters for EVERY configured model."""

    index: int
    models: dict[str, LoadedModel] = field(default_factory=dict)


class PartitionWorker(worker_pb2_grpc.PartitionWorkerServicer):
    """A request takes a free slot, runs, and puts it back.

    More gRPC threads than slots: an extra request waits in `free` (the wait is
    returned as queue_wait_ms) instead of being rejected by gRPC. Admission
    control lives in the Go server; the wait here should normally be ~0.
    """

    def __init__(self, models: list[ModelConfig], slots: int, threads: int):
        if slots < 1 or threads < 1:
            raise ValueError("slots and threads must be >= 1")
        self.threads = threads
        self.hashes = {m.model_key: model_hash(m.model_dir) for m in models}
        self.slots = [
            Slot(i, {m.model_key: LoadedModel(m.model_key, m.model_dir, threads) for m in models})
            for i in range(slots)
        ]
        # queue.Queue is thread-safe; get() blocks until a slot is returned.
        self.free: queue.Queue[Slot] = queue.Queue()
        for slot in self.slots:
            self.free.put(slot)

    def Describe(self, request, context):
        # All slots load the same files: slot 0 is representative.
        first = self.slots[0]
        models = []
        for key, loaded in first.models.items():
            segments = [
                worker_pb2.Segment(
                    index=s.index,
                    input_shape=list(s.input_shape),
                    input_dtype=s.input_dtype.name,
                    input_bytes=s.input_bytes,
                    output_shape=list(s.output_shape),
                    output_bytes=s.output_bytes,
                )
                for s in loaded.specs()
            ]
            models.append(
                worker_pb2.ModelDescription(
                    model_key=key, model_hash=self.hashes[key], segments=segments
                )
            )
        return worker_pb2.DescribeResponse(
            models=models, slots=len(self.slots), threads=self.threads
        )

    def RunFrom(self, request, context):
        key = request.model_key
        if key not in self.hashes:
            # abort() raises: nothing below runs for an unknown model.
            context.abort(grpc.StatusCode.NOT_FOUND, f"unknown model {key!r}")

        # time_remaining() is None (or a huge value) without a deadline, and
        # can be <= 0 when it already expired: clamp to [0, MAX_SLOT_WAIT_S].
        remaining = context.time_remaining()
        timeout = (
            MAX_SLOT_WAIT_S if remaining is None else min(max(remaining, 0.0), MAX_SLOT_WAIT_S)
        )

        t0 = time.perf_counter()
        try:
            slot = self.free.get(timeout=timeout)
        except queue.Empty:
            context.abort(grpc.StatusCode.DEADLINE_EXCEEDED, "no free slot before the deadline")
        queue_wait_ms = (time.perf_counter() - t0) * 1000

        try:
            output, layer_ms = slot.models[key].run_from(request.split_point, request.tensor)
        except ValueError as e:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(e))
        finally:
            # Always give the slot back, or the pool shrinks for ever.
            self.free.put(slot)

        return worker_pb2.RunResponse(
            output=output, layer_ms=layer_ms, queue_wait_ms=queue_wait_ms, slot=slot.index
        )


def build_server(worker: PartitionWorker, listen: str) -> tuple[grpc.Server, int]:
    """A gRPC server for `worker` and the port it is bound to.

    `listen` is host:port, e.g. 127.0.0.1:50051; port 0 picks a free port (tests).
    """
    from concurrent import futures

    options = [
        ("grpc.max_receive_message_length", MAX_MESSAGE_BYTES),
        ("grpc.max_send_message_length", MAX_MESSAGE_BYTES),
    ]
    server = grpc.server(
        futures.ThreadPoolExecutor(max_workers=len(worker.slots) * 4), options=options
    )
    worker_pb2_grpc.add_PartitionWorkerServicer_to_server(worker, server)
    port = server.add_insecure_port(listen)
    if port == 0:
        raise OSError(f"cannot listen on {listen}")
    return server, port
