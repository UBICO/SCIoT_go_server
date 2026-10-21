"""sciot-worker tune: find slots × threads for the machine it runs on.

    uv run sciot-worker tune --model yolo_small_320=/path/to/yolo_small_320

The right worker size depends on the server machine (its cores, whether some
are reserved or efficiency cores, its memory), so the tool reads the machine
itself and measures it: run it ON the server, where the Go server will run.

For every candidate combination it starts a real ``sciot-worker`` process,
as the Go server does, and loads it over gRPC from this process:

- 1 request at a time → the latency of a single frame, which decides
  whether SPLIT beats LOCAL within the frame's deadline;
- as many requests as slots → the throughput. The Go dispatcher never sends
  the worker more requests than its slots, so there is no point going beyond.

Output: a table, three choices (lowest latency, highest throughput, and the
recommended compromise: the highest throughput among the combinations whose
latency is within 10 % of the best), the config.json snippet, and a JSON
file with every number and the machine description, to take away.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import platform
import socket
import subprocess
import sys
import threading
import time
from collections.abc import Callable, Sequence
from dataclasses import asdict, dataclass, field
from datetime import datetime
from pathlib import Path

import grpc
import numpy as np

from sciot_worker.server import MAX_MESSAGE_BYTES
from sciot_worker.v1 import worker_pb2, worker_pb2_grpc

# A combination is kept only if it uses at least this share of the cores:
# fewer threads than cores leaves the machine idle.
MIN_CORE_FILL = 0.75
# The recommended combination may be this much slower per frame than the best.
LATENCY_TOLERANCE = 0.10
# Outputs of different combinations must agree (threads must not change results).
OUTPUT_ATOL = 1e-4


# ---------------------------------------------------------------------------
# Machine
# ---------------------------------------------------------------------------


@dataclass
class Machine:
    """What the tool knows about the machine. None = could not be read."""

    system: str
    arch: str
    logical_cores: int
    usable_cores: int
    usable_source: str  # cpu_count, affinity or cgroup: what limited usable_cores
    physical_cores: int | None = None
    performance_cores: int | None = None
    efficiency_cores: int | None = None
    memory_bytes: int | None = None
    memory_kind: str | None = None  # "available" (Linux) or "total" (macOS)


def _read(path: Path) -> str | None:
    try:
        return path.read_text().strip()
    except OSError:
        return None


def _sysctl(name: str) -> int | None:
    try:
        out = subprocess.run(["sysctl", "-n", name], capture_output=True, text=True, timeout=2)
        return int(out.stdout.strip()) if out.returncode == 0 else None
    except (OSError, ValueError, subprocess.SubprocessError):
        return None


def cgroup_cpu_limit(cpu_max: str | None) -> int | None:
    """Cores allowed by a cgroup v2 ``cpu.max`` ("max 100000" or "200000 100000")."""
    if not cpu_max:
        return None
    parts = cpu_max.split()
    if len(parts) != 2 or parts[0] == "max":
        return None
    try:
        quota, period = int(parts[0]), int(parts[1])
    except ValueError:
        return None
    return max(1, math.ceil(quota / period)) if quota > 0 and period > 0 else None


def parse_cpu_list(text: str | None) -> int | None:
    """Number of CPUs in a Linux cpu list such as "0-3,8,10-11"."""
    if not text:
        return None
    count = 0
    for part in text.split(","):
        lo, _, hi = part.partition("-")
        try:
            count += (int(hi) if hi else int(lo)) - int(lo) + 1
        except ValueError:
            return None
    return count


def linux_physical_cores(cpu_root: Path) -> int | None:
    """Distinct (package, core) pairs: hyper-threads of one core count once."""
    cores = set()
    for topo in cpu_root.glob("cpu[0-9]*/topology"):
        package, core = _read(topo / "physical_package_id"), _read(topo / "core_id")
        if package is None or core is None:
            return None
        cores.add((package, core))
    return len(cores) or None


def detect_machine(
    *,
    cpu_root: Path = Path("/sys/devices/system/cpu"),
    devices_root: Path = Path("/sys/devices"),
    cgroup_root: Path = Path("/sys/fs/cgroup"),
    meminfo: Path = Path("/proc/meminfo"),
) -> Machine:
    """Describe this machine with the standard library only."""
    logical = os.cpu_count() or 1
    m = Machine(platform.system(), platform.machine(), logical, logical, "cpu_count")

    if hasattr(os, "sched_getaffinity"):  # Linux: the CPUs this process may use
        affinity = len(os.sched_getaffinity(0))
        if affinity < m.usable_cores:
            m.usable_cores, m.usable_source = affinity, "affinity"
    limit = cgroup_cpu_limit(_read(cgroup_root / "cpu.max"))  # containers, systemd slices
    if limit is not None and limit < m.usable_cores:
        m.usable_cores, m.usable_source = limit, "cgroup"

    if m.system == "Darwin":
        m.physical_cores = _sysctl("hw.physicalcpu")
        m.performance_cores = _sysctl("hw.perflevel0.physicalcpu")
        m.efficiency_cores = _sysctl("hw.perflevel1.physicalcpu")
        m.memory_bytes, m.memory_kind = _sysctl("hw.memsize"), "total"
    elif m.system == "Linux":
        m.physical_cores = linux_physical_cores(cpu_root)
        # Intel hybrid CPUs expose their P and E cores as two PMU devices.
        m.performance_cores = parse_cpu_list(_read(devices_root / "cpu_core" / "cpus"))
        m.efficiency_cores = parse_cpu_list(_read(devices_root / "cpu_atom" / "cpus"))
        text = _read(meminfo) or ""
        for line in text.splitlines():
            if line.startswith("MemAvailable:"):
                m.memory_bytes, m.memory_kind = int(line.split()[1]) * 1024, "available"
    return m


# ---------------------------------------------------------------------------
# Candidates
# ---------------------------------------------------------------------------


def candidate_combos(cores: int, min_fill: float = MIN_CORE_FILL) -> list[tuple[int, int]]:
    """(slots, threads) that use the cores without exceeding them.

    For every number of slots, the most threads that fit; kept only if the
    combination uses at least min_fill of the cores. 12 cores → 12×1, 6×2,
    4×3, 3×4, 2×6, 1×12.
    """
    best: dict[int, int] = {}
    for threads in range(1, cores + 1):
        slots = cores // threads
        if slots * threads >= min_fill * cores:
            best[slots] = max(best.get(slots, 0), threads)
    return sorted(best.items(), reverse=True)


def parse_combos(value: str) -> list[tuple[int, int]]:
    """ "2x6,3x4" → [(2, 6), (3, 4)]."""
    combos = []
    for item in value.split(","):
        try:
            slots, threads = (int(x) for x in item.lower().split("x"))
        except ValueError:
            raise ValueError(f"{item!r}: expected SLOTSxTHREADS, e.g. 2x6") from None
        if slots < 1 or threads < 1:
            raise ValueError(f"{item!r}: slots and threads must be >= 1")
        combos.append((slots, threads))
    return combos


# ---------------------------------------------------------------------------
# Measurement
# ---------------------------------------------------------------------------


@dataclass
class Load:
    concurrency: int
    requests: int
    seconds: float
    throughput_rps: float
    latency_ms_p50: float
    latency_ms_p95: float


@dataclass
class ComboResult:
    slots: int
    threads: int
    split_point: int
    startup_s: float
    rss_bytes: int | None
    single: Load  # 1 request at a time
    full: Load  # as many requests as slots
    output_max_abs_diff: float | None = None  # against the first combination
    warnings: list[str] = field(default_factory=list)

    @property
    def label(self) -> str:
        return f"{self.slots}x{self.threads}"


def percentile(values: Sequence[float], q: float) -> float:
    """q-th percentile (0..100), linear interpolation."""
    ordered = sorted(values)
    if not ordered:
        return float("nan")
    k = (len(ordered) - 1) * q / 100
    lo, hi = math.floor(k), math.ceil(k)
    return ordered[lo] + (ordered[hi] - ordered[lo]) * (k - lo)


def run_load(call: Callable[[], None], concurrency: int, seconds: float) -> Load:
    """``concurrency`` closed-loop clients calling ``call`` for ``seconds``."""
    latencies: list[float] = []
    lock = threading.Lock()
    start = time.perf_counter()
    end = start + seconds

    def client() -> None:
        mine = []
        while time.perf_counter() < end:
            t0 = time.perf_counter()
            call()
            mine.append((time.perf_counter() - t0) * 1000)
        with lock:
            latencies.extend(mine)

    threads = [threading.Thread(target=client) for _ in range(concurrency)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    elapsed = time.perf_counter() - start
    return Load(
        concurrency,
        len(latencies),
        elapsed,
        len(latencies) / elapsed,
        percentile(latencies, 50),
        percentile(latencies, 95),
    )


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def rss_bytes(pid: int) -> int | None:
    """Resident memory of a process (ps works on Linux and macOS)."""
    try:
        out = subprocess.run(
            ["ps", "-o", "rss=", "-p", str(pid)], capture_output=True, text=True, timeout=2
        )
        return int(out.stdout.strip()) * 1024
    except (OSError, ValueError, subprocess.SubprocessError):
        return None


def start_worker(models: Sequence[str], slots: int, threads: int, port: int) -> subprocess.Popen:
    """A real sciot-worker process, started like the Go server starts it."""
    cmd = [
        sys.executable,
        "-m",
        "sciot_worker",
        "--listen",
        f"127.0.0.1:{port}",
        "--slots",
        str(slots),
        "--threads",
        str(threads),
    ]
    for m in models:
        cmd += ["--model", m]
    return subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)


def wait_ready(stub, proc: subprocess.Popen, timeout_s: float) -> worker_pb2.DescribeResponse:
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(f"worker exited with code {proc.returncode}:\n{proc.stderr.read()}")
        try:
            return stub.Describe(worker_pb2.DescribeRequest(), timeout=1)
        except grpc.RpcError:
            time.sleep(0.2)
    raise RuntimeError(f"worker not ready after {timeout_s} s")


def measure_combo(
    models: Sequence[str],
    model_key: str,
    split_point: int,
    slots: int,
    threads: int,
    *,
    warmup_s: float,
    seconds: float,
    reference: bytes | None,
    startup_timeout_s: float = 300,
) -> tuple[ComboResult, bytes]:
    """Start a worker with this size, load it, stop it. Returns the result
    and its raw output for the fixed input (to compare combinations)."""
    port = free_port()
    started = time.perf_counter()
    proc = start_worker(models, slots, threads, port)
    channel = grpc.insecure_channel(
        f"127.0.0.1:{port}",
        options=[
            ("grpc.max_send_message_length", MAX_MESSAGE_BYTES),
            ("grpc.max_receive_message_length", MAX_MESSAGE_BYTES),
        ],
    )
    try:
        stub = worker_pb2_grpc.PartitionWorkerStub(channel)
        desc = wait_ready(stub, proc, startup_timeout_s)
        startup_s = time.perf_counter() - started
        model = next((m for m in desc.models if m.model_key == model_key), None)
        if model is None:
            raise ValueError(f"model {model_key!r} not loaded by the worker")
        segment = next((s for s in model.segments if s.index == split_point + 1), None)
        if segment is None:
            raise ValueError(f"split point {split_point} out of range for {model_key!r}")
        # Fixed seed: every combination computes the same input.
        tensor = (
            np.random.default_rng(20261021)
            .random(segment.input_bytes // 4, dtype=np.float32)
            .tobytes()
        )
        request = worker_pb2.RunRequest(model_key=model_key, split_point=split_point, tensor=tensor)

        output = stub.RunFrom(
            worker_pb2.RunRequest(
                model_key=model_key, split_point=split_point, tensor=tensor, want_raw=True
            )
        ).output
        diff = None
        if reference is not None:
            a = np.frombuffer(output, dtype=np.float32)
            b = np.frombuffer(reference, dtype=np.float32)
            diff = float(np.max(np.abs(a - b))) if a.shape == b.shape else float("inf")

        def call() -> None:
            stub.RunFrom(request)

        run_load(call, slots, warmup_s)  # interpreters, caches, CPU frequency
        single = run_load(call, 1, seconds)
        full = run_load(call, slots, seconds)
        result = ComboResult(
            slots, threads, split_point, startup_s, rss_bytes(proc.pid), single, full, diff
        )
        if diff is not None and diff > OUTPUT_ATOL:
            result.warnings.append(f"output differs from the first combination by {diff:.3g}")
        return result, output
    finally:
        channel.close()
        proc.terminate()
        try:
            proc.wait(10)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()


# ---------------------------------------------------------------------------
# Choice
# ---------------------------------------------------------------------------


def recommend(results: Sequence[ComboResult], tolerance: float = LATENCY_TOLERANCE) -> dict:
    """Lowest single-frame latency, highest throughput, and the compromise:
    the highest throughput among those within ``tolerance`` of the best latency."""
    if not results:
        return {}
    fastest = min(results, key=lambda r: r.single.latency_ms_p50)
    widest = max(results, key=lambda r: r.full.throughput_rps)
    limit = fastest.single.latency_ms_p50 * (1 + tolerance)
    close = [r for r in results if r.single.latency_ms_p50 <= limit]
    balanced = max(close, key=lambda r: r.full.throughput_rps)
    return {
        "lowest_latency": fastest.label,
        "highest_throughput": widest.label,
        "recommended": balanced.label,
        "tolerance": tolerance,
    }


def format_table(results: Sequence[ComboResult]) -> str:
    lines = [
        "| slots × threads | 1 request: p50 / p95 ms | slots requests: p50 / p95 ms "
        "| throughput req/s | worker RSS MiB |",
        "|---|---|---|---|---|",
    ]
    for r in results:
        rss = "—" if r.rss_bytes is None else f"{r.rss_bytes / 2**20:.0f}"
        lines.append(
            f"| {r.slots} × {r.threads} | {r.single.latency_ms_p50:.1f} / {r.single.latency_ms_p95:.1f} "
            f"| {r.full.latency_ms_p50:.1f} / {r.full.latency_ms_p95:.1f} "
            f"| {r.full.throughput_rps:.1f} | {rss} |"
        )
    return "\n".join(lines)


def describe_machine(m: Machine) -> str:
    def opt(v: int | None) -> str:
        return "unknown" if v is None else str(v)

    mem = (
        "unknown" if m.memory_bytes is None else f"{m.memory_bytes / 2**30:.1f} GiB {m.memory_kind}"
    )
    return (
        f"{m.system} {m.arch}: {m.logical_cores} logical cores, {m.usable_cores} usable "
        f"({m.usable_source}), physical {opt(m.physical_cores)}, "
        f"performance {opt(m.performance_cores)}, efficiency {opt(m.efficiency_cores)}, "
        f"memory {mem}"
    )


# ---------------------------------------------------------------------------
# Command
# ---------------------------------------------------------------------------


def main(argv: Sequence[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="sciot-worker tune", description=__doc__.split("\n")[0])
    p.add_argument(
        "--model",
        action="append",
        required=True,
        metavar="KEY=DIR",
        help="the model(s) the server will serve, as for sciot-worker; the first one is measured",
    )
    p.add_argument(
        "--splits",
        default="-1",
        help="split points to load (default -1: the whole model); a list starting with "
        "a negative number needs the = form, e.g. --splits=-1,6",
    )
    p.add_argument(
        "--combos", help='SLOTSxTHREADS list instead of the automatic one, e.g. "2x6,3x4"'
    )
    p.add_argument("--seconds", type=float, default=8.0, help="seconds per measurement")
    p.add_argument("--warmup", type=float, default=2.0, help="seconds of warm-up per combination")
    p.add_argument("--out", type=Path, help="JSON results (default: tune-<host>-<time>.json here)")
    args = p.parse_args(argv)

    machine = detect_machine()
    print(describe_machine(machine))
    combos = parse_combos(args.combos) if args.combos else candidate_combos(machine.usable_cores)
    splits = [int(s) for s in args.splits.split(",")]
    model_key = args.model[0].partition("=")[0]
    total = len(combos) * len(splits) * (args.warmup + 2 * args.seconds)
    print(
        f"{len(combos)} combinations × {len(splits)} split(s): about {total / 60:.1f} min "
        f"plus one worker start each"
    )
    for slots, threads in combos:
        if slots * threads > machine.usable_cores:
            print(
                f"WARNING: {slots}x{threads} uses {slots * threads} threads on "
                f"{machine.usable_cores} usable cores"
            )

    by_split: dict[int, list[ComboResult]] = {}
    for split in splits:
        reference = None
        results = []
        for slots, threads in combos:
            print(f"split {split}: {slots}x{threads} ...", flush=True)
            r, output = measure_combo(
                args.model,
                model_key,
                split,
                slots,
                threads,
                warmup_s=args.warmup,
                seconds=args.seconds,
                reference=reference,
            )
            reference = reference if reference is not None else output
            results.append(r)
            for w in r.warnings:
                print(f"  WARNING: {w}")
        by_split[split] = results

    report = {
        "machine": asdict(machine),
        "model": args.model,
        "settings": {"seconds": args.seconds, "warmup": args.warmup, "splits": splits},
        "results": {
            str(s): [asdict(r) | {"label": r.label} for r in rs] for s, rs in by_split.items()
        },
        "choice": {str(s): recommend(rs) for s, rs in by_split.items()},
    }
    for split, results in by_split.items():
        print(
            f"\nsplit {split} ({'whole model' if split == -1 else f'segments {split + 1}..last'}):"
        )
        print(format_table(results))
        c = report["choice"][str(split)]
        print(
            f"lowest latency {c['lowest_latency']}, highest throughput "
            f"{c['highest_throughput']}, recommended {c['recommended']} "
            f"(highest throughput within {c['tolerance']:.0%} of the best latency)"
        )

    first = report["choice"][str(splits[0])]["recommended"]
    slots, threads = (int(x) for x in first.split("x"))
    snippet = {"worker": {"slots": slots, "threads": threads}}
    report["config_snippet"] = snippet
    print(f"\nconfig.json: {json.dumps(snippet)}")

    out = args.out or Path(f"tune-{platform.node()}-{datetime.now():%Y%m%d-%H%M%S}.json")
    out.write_text(json.dumps(report, indent=2) + "\n")
    print(f"results in {out}")
    return 0
