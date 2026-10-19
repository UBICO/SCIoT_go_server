"""Command line entry point.

    sciot-worker --listen 127.0.0.1:50051 --slots 2 --threads 6 \\
        --model yolo_small_320=/path/to/yolo-small --model fomo_96x96=/path/to/tflite_96

The Go server starts the worker with these arguments, built from its own
config.json: the worker has no configuration file of its own.
"""

from __future__ import annotations

import argparse
import signal
import sys
import threading
import time
from pathlib import Path

from sciot_worker.server import ModelConfig, PartitionWorker, build_server


def parse_model(value: str) -> ModelConfig:
    key, sep, path = value.partition("=")
    if not sep or not key or not path:
        raise argparse.ArgumentTypeError(f"expected KEY=DIR, got {value!r}")
    return ModelConfig(key, Path(path))


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="sciot-worker", description=__doc__.split("\n")[0])
    parser.add_argument("--listen", required=True, help="host:port, e.g. 127.0.0.1:50051")
    parser.add_argument("--slots", type=int, default=1)
    parser.add_argument("--threads", type=int, default=1, help="TFLite threads per interpreter")
    parser.add_argument(
        "--model", type=parse_model, action="append", required=True, metavar="KEY=DIR"
    )
    args = parser.parse_args(argv)

    started = time.perf_counter()
    worker = PartitionWorker(args.model, args.slots, args.threads)
    server, _ = build_server(worker, args.listen)
    server.start()

    stop = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stop.set())
    signal.signal(signal.SIGINT, lambda *_: stop.set())
    # The Go server does not parse this line (it waits for Describe), but it is
    # useful in the logs.
    print(
        f"sciot-worker listening on {args.listen}: {len(args.model)} model(s), "
        f"{args.slots} slot(s) x {args.threads} thread(s), "
        f"loaded in {(time.perf_counter() - started) * 1000:.0f} ms",
        file=sys.stderr,
        flush=True,
    )
    stop.wait()
    server.stop(grace=2).wait()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
