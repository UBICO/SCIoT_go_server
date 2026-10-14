"""gRPC worker: runs TFLite segment chains for the Go server (spike 2.2).

    python worker.py --model-dir ../yolo-small --listen unix:/tmp/sciot-w0.sock --slots 2 --threads 2

A *slot* is a complete set of interpreters. The gRPC thread pool can accept
more requests than there are slots; a request waits for a free slot (that wait
is returned as queue_wait_ms), so a slot is never used by two requests at once.
"""

from __future__ import annotations

import argparse
import queue
import signal
import sys
import threading
import time
from concurrent import futures
from pathlib import Path

import grpc

# Generated code imports "worker_pb2" as a top-level module.
sys.path.insert(0, str(Path(__file__).resolve().parent))

import worker_pb2  # noqa: E402
import worker_pb2_grpc  # noqa: E402
from segments import Slot, run_chain, segment_paths  # noqa: E402

MAX_MESSAGE_BYTES = 64 << 20  # gRPC default is 4 MiB; the largest activation is 3.2 MB


class PartitionWorker(worker_pb2_grpc.PartitionWorkerServicer):
    def __init__(self, model_key: str, slots: list[Slot]):
        self.model_key = model_key
        # queue.Queue is thread-safe: get() blocks until a slot is put back.
        self.free = queue.Queue()
        for slot in slots:
            self.free.put(slot)

    def RunFrom(self, request, context):
        if request.model_key and request.model_key != self.model_key:
            context.abort(grpc.StatusCode.NOT_FOUND, f"model {request.model_key!r} not served")

        waited = time.perf_counter()
        slot = self.free.get()
        queue_wait_ms = (time.perf_counter() - waited) * 1000
        try:
            output, layer_ms = run_chain(slot, request.split_point, request.tensor)
        except ValueError as exc:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(exc))
        finally:
            # Always give the slot back, even when the request failed.
            self.free.put(slot)
        return worker_pb2.RunResponse(
            output=output, layer_ms=layer_ms, queue_wait_ms=queue_wait_ms, slot=slot.index
        )


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("--model-dir", required=True)
    parser.add_argument("--model-key", default="bench")
    parser.add_argument("--listen", required=True, help="unix:/path.sock or host:port")
    parser.add_argument("--slots", type=int, default=1)
    parser.add_argument("--threads", type=int, default=1, help="TFLite threads per interpreter")
    args = parser.parse_args(argv)

    started = time.perf_counter()
    paths = segment_paths(args.model_dir)
    slots = [Slot(i, paths, args.threads) for i in range(args.slots)]
    load_ms = (time.perf_counter() - started) * 1000

    options = [
        ("grpc.max_receive_message_length", MAX_MESSAGE_BYTES),
        ("grpc.max_send_message_length", MAX_MESSAGE_BYTES),
    ]
    # More gRPC threads than slots: extra requests wait in self.free (and the
    # wait is measured) instead of being rejected by gRPC.
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=args.slots * 4), options=options)
    worker_pb2_grpc.add_PartitionWorkerServicer_to_server(
        PartitionWorker(args.model_key, slots), server
    )
    server.add_insecure_port(args.listen)
    server.start()

    stop = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stop.set())
    signal.signal(signal.SIGINT, lambda *_: stop.set())
    # The run script waits for this line before starting the client.
    print(
        f"READY listen={args.listen} slots={args.slots} threads={args.threads} "
        f"segments={len(paths)} load_ms={load_ms:.0f}",
        flush=True,
    )
    stop.wait()
    server.stop(grace=2).wait()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
