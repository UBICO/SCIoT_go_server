"""Command line entry point.

    sciot-worker --listen 127.0.0.1:50051 --slots 2 --threads 6 \\
        --model yolo_small_320=/path/to/yolo-small --model fomo_96x96=/path/to/tflite_96

The Go server starts the worker with these arguments, built from its own
config.json: the worker has no configuration file of its own.

    sciot-worker tune --model yolo_small_320=/path/to/yolo-small

measures slots × threads on this machine (sciot_worker/tune.py).
"""

from __future__ import annotations

import argparse
import signal
import sys
import time
from pathlib import Path

from sciot_worker.server import ModelConfig, PartitionWorker, build_server


def parse_model(value: str) -> ModelConfig:
    key, sep, path = value.partition("=")
    if not sep or not key or not path:
        raise argparse.ArgumentTypeError(f"expected KEY=DIR, got {value!r}")
    return ModelConfig(key, Path(path))


def main(argv: list[str] | None = None) -> int:
    argv = sys.argv[1:] if argv is None else argv
    # A subcommand only for tune: the Go server starts the worker with plain
    # options (--listen ...), and that must keep working unchanged.
    if argv[:1] == ["tune"]:
        from sciot_worker import tune

        return tune.main(argv[1:])

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

    # The Go server does not parse this line (it waits for Describe), but it is
    # useful in the logs.
    print(
        f"sciot-worker listening on {args.listen}: {len(args.model)} model(s), "
        f"{args.slots} slot(s) x {args.threads} thread(s), "
        f"loaded in {(time.perf_counter() - started) * 1000:.0f} ms",
        file=sys.stderr,
        flush=True,
    )
    wait_for_stop()
    server.stop(grace=2).wait()
    return 0


def wait_for_stop(poll_s: float = 0.2) -> None:
    """Block until SIGTERM or SIGINT.

    The handler must not take a lock. With threading.Event.set() in the
    handler, a second signal arriving while the first handler held the
    Event's lock ran a nested handler on the same thread, which waited for
    that lock forever: on shutdown the Go server sends SIGTERM to the
    process group and `uv run` forwards another, and every worker of the
    phase 4.2 campaign stayed alive, deadlocked, still listening on its
    port (54 of 60 hung with a burst of SIGTERMs; 0 with a flag).
    """
    stopped = False

    def handler(*_):
        nonlocal stopped
        stopped = True

    signal.signal(signal.SIGTERM, handler)
    signal.signal(signal.SIGINT, handler)
    while not stopped:
        time.sleep(poll_s)


if __name__ == "__main__":
    raise SystemExit(main())
