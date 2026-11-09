"""SPLIT benchmark: every configuration of one model, Go server and Go client.

For LOCAL and every split point -1..last-1 it measures what the device gets:
frames per second, latency (capture → outcome), where the time goes
(device, server, network), bytes sent, and how many frames the server could
not take (finished locally by the device).

Method
------
One Go server (with its worker, ``-admin`` and recording) and ONE client,
which never restarts: the benchmark changes the policy from the admin
endpoint, as phase 3's algorithm will. For every configuration, ``--runs``
times in alternating order (spreads thermal drift and background load):

1. set the policy; wait for the first frame computed with its version
   (``--frames-csv`` of the client), so the client has applied it;
2. wait ``--warmup`` seconds, reset the byte-counting proxy;
3. measure for ``--measure`` seconds.

Frames are attributed to a configuration by their ``policy_version``, never
by time, so policy transitions cannot mix them. Device data come from the
client's frames CSV; queue waits and the dispatcher's prediction errors from
the server's recording (docs/RECORDING.md).

The model directory must be built by ``sciot-model build`` (manifest.json)
and is used by both the server and the client (same model_hash).

Example::

    uv run --project client python client/scripts/bench_split.py \\
        --model-dir ~/sciot-models/yolo_small_320

Results: ``client/results/split_bench/<time>/`` (or under $SCIOT_RESULTS_DIR):
results.json, results.md, plots, and the raw frames.csv and recording.
"""

from __future__ import annotations

import argparse
import csv
import json
import shutil
import signal
import statistics
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from collections import Counter
from dataclasses import asdict, dataclass
from datetime import datetime
from pathlib import Path
from typing import Any

sys.path.insert(0, str(Path(__file__).resolve().parent))
from bench_common import (
    GO_REPO,
    RESULTS_DIR,
    CountingProxy,
    log_tail,
    summarize,
    wait_http_ok,
)

LOCAL = "LOCAL"
# The client's static image: every benchmark result is computed on it.
BENCH_IMAGE = Path(__file__).resolve().parents[1] / "sciot_client" / "stop.png"


# ---------------------------------------------------------------------------
# Configurations and schedule
# ---------------------------------------------------------------------------


def configurations(num_segments: int, only: list[str] | None = None) -> list[str]:
    """LOCAL, then split points -1..last-1 (as strings, e.g. "-1", "4").

    ``only`` restricts and orders them (e.g. ["LOCAL", "-1", "4"]).
    """
    all_configs = [LOCAL] + [str(k) for k in range(-1, num_segments - 1)]
    if only is None:
        return all_configs
    unknown = [c for c in only if c not in all_configs]
    if unknown:
        raise ValueError(f"unknown configurations {unknown}; valid: {all_configs}")
    return only


def schedule(configs: list[str], runs: int) -> list[tuple[int, str]]:
    """(run, config) in alternating order: forward, backward, forward, ..."""
    order: list[tuple[int, str]] = []
    for run in range(1, runs + 1):
        sequence = configs if run % 2 else list(reversed(configs))
        order += [(run, c) for c in sequence]
    return order


def policy_body(config: str) -> dict:
    if config == LOCAL:
        return {"mode": "LOCAL", "split_point": None}
    return {"mode": "SPLIT", "split_point": int(config)}


def go_config(
    model_dir: Path,
    go_repo: Path | None,
    worker_port: int,
    slots: int,
    threads: int,
    record_dir: Path,
    heartbeat_ms: int,
) -> dict:
    """config.json of the Go server: one model, managed worker, recording."""
    worker: dict[str, Any] = {"address": f"127.0.0.1:{worker_port}"}
    if go_repo is not None:
        worker.update(
            command=["uv", "run", "--project", str(go_repo / "worker"), "sciot-worker"],
            slots=slots,
            threads=threads,
        )
    return {
        "server": {"heartbeat_interval_ms": heartbeat_ms},
        "worker": worker,
        "models": [{"model_key": f"bench_{model_dir.name}", "tflite_dir": str(model_dir)}],
        "record": {"dir": str(record_dir)},
    }


def client_config(server: str, device_id: str, model_dir: Path) -> dict:
    """go_client.yaml of the benchmark client: everything explicit, so a
    locally edited go_client.yaml (device, image) cannot change the results."""
    return {
        "server": server,
        "device_id": device_id,
        "model_dir": str(model_dir),
        "image": str(BENCH_IMAGE),
        "probe": True,
        "camera": "static",  # repeatable: the same frame every cycle
        "camera_index": 0,
    }


# ---------------------------------------------------------------------------
# Metrics (pure functions: tested without servers)
# ---------------------------------------------------------------------------


@dataclass
class Phase:
    run: int
    config: str
    policy_version: int
    first_t_s: float = 0.0  # client time of the first frame with this version
    up_bytes: int = 0  # through the proxy during the measure window
    down_bytes: int = 0


def _floats(rows: list[dict], column: str) -> list[float]:
    return [float(r[column]) for r in rows if r.get(column) not in (None, "")]


def _p(values: list[float], q: float) -> float | None:
    """Percentile q (0..100) by linear interpolation; None without values."""
    if not values:
        return None
    if len(values) == 1:
        return values[0]
    return statistics.quantiles(values, n=100, method="inclusive")[int(q) - 1]


def phase_frames(rows: list[dict], phase: Phase, warmup_s: float, measure_s: float) -> list[dict]:
    """The frames of a phase inside its measure window (client clock)."""
    start = phase.first_t_s + warmup_s
    end = start + measure_s
    return [
        r
        for r in rows
        if int(r["policy_version"]) == phase.policy_version and start <= float(r["t_s"]) < end
    ]


def phase_metrics(frames: list[dict], measure_s: float, reference_detections: int | None) -> dict:
    """What the device got in one phase."""
    outcomes = Counter(r["outcome"] for r in frames)
    results = [r for r in frames if r["outcome"] == "result"]
    n = len(frames)
    detections = [int(r["detections"]) for r in frames if r.get("detections") not in (None, "")]
    return {
        "frames": n,
        "fps": n / measure_s,
        "latency_ms_p50": _p(_floats(frames, "latency_ms"), 50),
        "latency_ms_p95": _p(_floats(frames, "latency_ms"), 95),
        # Device time of the frames the server answered (the prefix of an
        # offloaded frame); for LOCAL, of every frame. Mixing in the frames
        # finished locally would make the median jump between two groups.
        "device_ms_p50": _p(_floats(results or frames, "dev_ms"), 50),
        "server_ms_p50": _p(_floats(results, "srv_ms"), 50),
        "network_ms_p50": _p(_floats(results, "net_ms"), 50),
        "sent_bytes_per_frame": (sum(int(r["sent_bytes"]) for r in frames) / n) if n else 0.0,
        "result_fraction": len(results) / n if n else 0.0,
        "outcomes": dict(outcomes),
        "detections_differ": (
            None
            if reference_detections is None
            else sum(1 for d in detections if d != reference_detections)
        ),
    }


def server_metrics(record_frames: list[dict], policy_version: int) -> dict:
    """Queue and admission, from the server's recording (frame events)."""
    frames = [f for f in record_frames if f.get("policy_version") == policy_version]
    results = [f for f in frames if f.get("outcome") == "result" and f.get("timing")]
    waits = [f["timing"]["queue_wait_ms"] for f in results if "queue_wait_ms" in f["timing"]]
    errors = [
        f["timing"]["queue_wait_ms"] - f["admission"]["predicted_wait_ms"]
        for f in results
        if f.get("admission") and "queue_wait_ms" in f["timing"]
    ]
    return {
        "server_frames": len(frames),
        "queue_wait_ms_p50": _p(waits, 50),
        "queue_wait_ms_p95": _p(waits, 95),
        "wait_prediction_error_ms_p50": _p(errors, 50),
        "wait_prediction_error_ms_p95": _p(errors, 95),
        "server_outcomes": dict(Counter(f.get("outcome") for f in frames)),
    }


def reference_detections(rows: list[dict]) -> int | None:
    """Detections of the LOCAL frames (most common count): the static image
    gives the same answer every time, so SPLIT frames must give it too."""
    counts = Counter(
        int(r["detections"])
        for r in rows
        if r["mode"] == LOCAL and r.get("detections") not in (None, "")
    )
    return counts.most_common(1)[0][0] if counts else None


NUMERIC = [
    "fps",
    "latency_ms_p50",
    "latency_ms_p95",
    "device_ms_p50",
    "server_ms_p50",
    "network_ms_p50",
    "sent_bytes_per_frame",
    "result_fraction",
    "queue_wait_ms_p50",
    "wait_prediction_error_ms_p50",
    "wait_prediction_error_ms_p95",
    "up_kib_per_s",
]


def summarize_configs(per_phase: list[dict]) -> dict[str, dict]:
    """Per configuration: mean/stdev over the runs of every numeric metric."""
    out: dict[str, dict] = {}
    for config in dict.fromkeys(p["config"] for p in per_phase):
        phases = [p for p in per_phase if p["config"] == config]
        summary: dict[str, Any] = {"runs": len(phases)}
        for metric in NUMERIC:
            values = [p[metric] for p in phases if p.get(metric) is not None]
            summary[metric] = summarize(values) if values else None
        summary["outcomes"] = dict(sum((Counter(p["outcomes"]) for p in phases), Counter()))
        differ = [p["detections_differ"] for p in phases if p.get("detections_differ") is not None]
        summary["detections_differ"] = sum(differ) if differ else None
        out[config] = summary
    return out


def format_table(summary: dict[str, dict]) -> str:
    """Markdown table: one row per configuration, mean ± stdev."""

    def cell(s: dict | None, fmt: str = ".1f") -> str:
        if s is None:
            return "—"
        return f"{s['mean']:{fmt}} ± {s['stdev']:{fmt}}"

    header = (
        "| Config | Frames/s | Latency p50 (ms) | Latency p95 (ms) | Device (ms) | "
        "Server (ms) | Network (ms) | Sent KiB/frame | Server results | Detections ≠ LOCAL |"
    )
    lines = [header, "|" + "---|" * 10]
    for config, s in summary.items():
        sent = s["sent_bytes_per_frame"]
        sent_kib = None if sent is None else {k: v / 1024 for k, v in sent.items() if k != "n"}
        results = s["result_fraction"]
        results_pct = (
            None if results is None else {k: v * 100 for k, v in results.items() if k != "n"}
        )
        lines.append(
            f"| {config if config == LOCAL else 'split ' + config} | {cell(s['fps'])} | "
            f"{cell(s['latency_ms_p50'])} | {cell(s['latency_ms_p95'])} | "
            f"{cell(s['device_ms_p50'])} | {cell(s['server_ms_p50'])} | "
            f"{cell(s['network_ms_p50'])} | {cell(sent_kib, '.0f')} | "
            f"{cell(results_pct, '.0f')} % | "
            f"{'—' if s['detections_differ'] is None else s['detections_differ']} |"
        )
    return "\n".join(lines)


# ---------------------------------------------------------------------------
# Processes
# ---------------------------------------------------------------------------


def http_json(
    method: str, url: str, body: dict | None = None, timeout: float = 5
) -> tuple[int, dict]:
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(url, data=data, method=method)
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.status, json.loads(response.read() or b"{}")
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read() or b"{}")


def set_policy(server: str, device_id: str, config: str) -> int:
    """Set the policy from the admin endpoint; returns its version."""
    status, body = http_json(
        "POST", f"{server}/api/v1/admin/devices/{device_id}/policy", policy_body(config)
    )
    if status != 200:
        hint = ""
        if config == "-1" and status == 400:
            hint = " (split -1 needs UBICO/SCIoT_go_server#16)"
        raise RuntimeError(f"set policy {config}: HTTP {status} {body}{hint}")
    return int(body["policy"]["policy_version"])


def read_frames(path: Path) -> list[dict]:
    if not path.exists():
        return []
    with path.open(newline="", encoding="utf-8") as f:
        return list(csv.DictReader(f))


def wait_first_frame(path: Path, version: int, timeout_s: float, client: subprocess.Popen) -> float:
    """Client time of the first frame computed with policy `version`."""
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        if client.poll() is not None:
            raise RuntimeError(f"client exited with code {client.returncode}")
        for row in read_frames(path):
            if int(row["policy_version"]) == version:
                return float(row["t_s"])
        time.sleep(0.1)
    raise RuntimeError(f"no frame with policy version {version} after {timeout_s} s")


def wait_registered(
    server: str, device_id: str, timeout_s: float, client: subprocess.Popen
) -> None:
    """Until the client is registered: the admin endpoint answers 200."""
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        if client.poll() is not None:
            raise RuntimeError(f"client exited with code {client.returncode}")
        status, _ = http_json(
            "POST",
            f"{server}/api/v1/admin/devices/{device_id}/policy",
            policy_body(LOCAL),
        )
        if status == 200:
            return
        time.sleep(0.5)
    raise RuntimeError(f"client {device_id!r} not registered after {timeout_s} s")


def wait_worker(server: str, timeout_s: float, proc: subprocess.Popen, log: Path) -> None:
    """Until the data plane is up: the status shows worker slots."""
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(f"server exited\n{log_tail(log)}")
        status, body = http_json("GET", f"{server}/api/v1/server/status")
        if status == 200 and (body.get("dispatch") or {}).get("slots", 0) > 0:
            return
        time.sleep(1)
    raise RuntimeError(f"worker not ready after {timeout_s} s\n{log_tail(log)}")


def stop(proc: subprocess.Popen | None, sig: int, timeout_s: float = 30) -> None:
    if proc is None or proc.poll() is not None:
        return
    proc.send_signal(sig)
    try:
        proc.wait(timeout_s)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()


def read_record(record_dir: Path) -> tuple[list[dict], Path | None]:
    files = sorted(record_dir.glob("*.jsonl"))
    if not files:
        return [], None
    lines = [json.loads(line) for line in files[-1].read_text().splitlines() if line.strip()]
    return [e for e in lines if e.get("kind") == "frame"], files[-1]


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def parse_args(argv: list[str] | None) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument(
        "--model-dir",
        type=Path,
        required=True,
        help="directory built by sciot-model build (server and client)",
    )
    p.add_argument(
        "--go-repo",
        type=Path,
        default=GO_REPO,
        help="SCIoT_go_server checkout: builds the server and runs its worker "
        "(default: the one this script is in)",
    )
    p.add_argument("--go-bin", type=Path, help="prebuilt Go server binary")
    p.add_argument("--python", default=sys.executable, help="interpreter with the client installed")
    p.add_argument("--configs", help='comma-separated subset, e.g. "LOCAL,-1,4" (default: all)')
    p.add_argument("--runs", type=int, default=3)
    p.add_argument("--warmup", type=float, default=5.0, help="seconds after each policy change")
    p.add_argument("--measure", type=float, default=20.0, help="seconds measured per phase")
    p.add_argument("--slots", type=int, default=2, help="worker slots")
    p.add_argument("--threads", type=int, default=2, help="TFLite threads per slot")
    p.add_argument("--heartbeat-ms", type=int, default=1000)
    p.add_argument("--go-port", type=int, default=18190)
    p.add_argument("--proxy-port", type=int, default=18191)
    p.add_argument("--worker-port", type=int, default=50190)
    p.add_argument("--device-id", default="bench_split")
    p.add_argument("--out", type=Path, help="results directory")
    args = p.parse_args(argv)
    args.model_dir = args.model_dir.expanduser().resolve()
    if not (args.model_dir / "manifest.json").is_file():
        p.error(f"{args.model_dir} has no manifest.json: build it with sciot-model build")
    args.go_repo = args.go_repo.expanduser().resolve()
    return args


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    manifest = json.loads((args.model_dir / "manifest.json").read_text())
    configs = configurations(
        len(manifest["segments"]), args.configs.split(",") if args.configs else None
    )
    order = schedule(configs, args.runs)
    total = len(order) * (args.warmup + args.measure)
    print(f"{len(configs)} configurations × {args.runs} runs: about {total / 60:.0f} min")

    out_dir = args.out or RESULTS_DIR / "split_bench" / datetime.now().strftime("%Y%m%d-%H%M%S")
    out_dir.mkdir(parents=True, exist_ok=True)
    server_url = f"http://127.0.0.1:{args.go_port}"
    server = client = proxy = None
    phases: list[Phase] = []

    with tempfile.TemporaryDirectory(prefix="sciot-split-bench-") as tmp_name:
        tmp = Path(tmp_name)
        record_dir = tmp / "record"
        frames_csv = tmp / "frames.csv"
        server_log, client_log = tmp / "server.log", tmp / "client.log"
        try:
            go_bin = args.go_bin
            if go_bin is None:
                go_bin = tmp / "sciot-server"
                subprocess.run(
                    ["go", "build", "-o", str(go_bin), "./cmd/server"],
                    cwd=args.go_repo,
                    check=True,
                )
            config_path = tmp / "config.json"
            config_path.write_text(
                json.dumps(
                    go_config(
                        args.model_dir,
                        args.go_repo,
                        args.worker_port,
                        args.slots,
                        args.threads,
                        record_dir,
                        args.heartbeat_ms,
                    )
                )
            )
            server = subprocess.Popen(
                [
                    str(go_bin),
                    "-config",
                    str(config_path),
                    "-addr",
                    f"127.0.0.1:{args.go_port}",
                    "-admin",
                ],
                stdout=server_log.open("w"),
                stderr=subprocess.STDOUT,
            )
            wait_http_ok(f"{server_url}/healthz", 60, proc=server, log_path=server_log)
            wait_worker(server_url, 180, server, server_log)
            print("server and worker ready")

            # The client talks through the proxy, which counts the bytes.
            proxy = CountingProxy(args.proxy_port, "127.0.0.1", args.go_port).start()
            client_config_path = tmp / "go_client.yaml"
            client_config_path.write_text(
                json.dumps(  # JSON is valid YAML
                    client_config(
                        f"http://127.0.0.1:{args.proxy_port}",
                        args.device_id,
                        args.model_dir,
                    )
                )
            )
            client = subprocess.Popen(
                [
                    args.python,
                    "-m",
                    "sciot_client.http_go_client",
                    "--config",
                    str(client_config_path),
                    "--no-table",
                    "--frames-csv",
                    str(frames_csv),
                ],
                stdout=client_log.open("w"),
                stderr=subprocess.STDOUT,
            )
            wait_registered(server_url, args.device_id, 120, client)
            print("client registered")

            for i, (run, config) in enumerate(order, 1):
                version = set_policy(server_url, args.device_id, config)
                phase = Phase(run, config, version)
                phase.first_t_s = wait_first_frame(frames_csv, version, 30, client)
                time.sleep(args.warmup)
                proxy.reset()
                time.sleep(args.measure)
                phase.up_bytes, phase.down_bytes = proxy.counts()
                phases.append(phase)
                print(f"[{i}/{len(order)}] run {run} {config:>5}: done")
        except BaseException:
            print(
                f"server log:\n{log_tail(server_log)}\nclient log:\n{log_tail(client_log)}",
                file=sys.stderr,
            )
            raise
        finally:
            stop(client, signal.SIGINT)  # the client prints its frame counters
            stop(server, signal.SIGTERM)  # the recording gets its "end" line
            if proxy is not None:
                proxy.stop()
            for path in (frames_csv, server_log, client_log):
                if path.exists():
                    shutil.copy(path, out_dir / path.name)

        record_frames, record_file = read_record(record_dir)
        if record_file is not None:
            shutil.copy(record_file, out_dir / "record.jsonl")

    rows = read_frames(out_dir / "frames.csv")
    reference = reference_detections(rows)
    per_phase = []
    for phase in phases:
        frames = phase_frames(rows, phase, args.warmup, args.measure)
        metrics = {
            **asdict(phase),
            **phase_metrics(frames, args.measure, reference),
            **server_metrics(record_frames, phase.policy_version),
            "up_kib_per_s": phase.up_bytes / 1024 / args.measure,
        }
        per_phase.append(metrics)
    summary = summarize_configs(per_phase)
    table = format_table(summary)

    results = {
        "model": {
            "dir": str(args.model_dir),
            "model_hash": manifest.get("model_hash"),
            "segments": len(manifest["segments"]),
        },
        "settings": {k: str(v) if isinstance(v, Path) else v for k, v in vars(args).items()},
        "reference_detections": reference,
        "phases": per_phase,
        "summary": summary,
    }
    (out_dir / "results.json").write_text(json.dumps(results, indent=2) + "\n")
    (out_dir / "results.md").write_text(table + "\n")
    plot(summary, out_dir)
    print(table)
    print(f"\nresults in {out_dir}")
    return 0


def plot(summary: dict[str, dict], out_dir: Path) -> None:
    """Latency and frames/s per configuration; time breakdown. Optional."""
    try:
        import matplotlib

        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
    except ImportError:
        print("matplotlib not installed: no plots")
        return
    labels = list(summary)
    x = range(len(labels))

    def mean(config: str, metric: str) -> float:
        s = summary[config][metric]
        return 0.0 if s is None else s["mean"]

    def err(metric: str) -> list[float]:
        return [0.0 if summary[c][metric] is None else summary[c][metric]["stdev"] for c in labels]

    fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(11, 4))
    ax1.bar(x, [mean(c, "fps") for c in labels], yerr=err("fps"), color="#4C72B0")
    ax1.set(
        title="Frames per second",
        xticks=list(x),
        xticklabels=labels,
        xlabel="configuration",
    )
    ax2.bar(
        x,
        [mean(c, "latency_ms_p50") for c in labels],
        yerr=err("latency_ms_p50"),
        color="#DD8452",
        label="p50",
    )
    ax2.plot(x, [mean(c, "latency_ms_p95") for c in labels], "ko", label="p95")
    ax2.set(title="Latency (ms)", xticks=list(x), xticklabels=labels, xlabel="configuration")
    ax2.legend()
    fig.tight_layout()
    fig.savefig(out_dir / "fps_latency.png", dpi=150)
    plt.close(fig)

    fig, ax = plt.subplots(figsize=(7, 4))
    bottom = [0.0] * len(labels)
    for metric, name, color in (
        ("device_ms_p50", "device", "#55A868"),
        ("server_ms_p50", "server", "#4C72B0"),
        ("network_ms_p50", "network", "#C44E52"),
    ):
        values = [mean(c, metric) for c in labels]
        ax.bar(x, values, bottom=bottom, label=name, color=color)
        bottom = [b + v for b, v in zip(bottom, values)]
    ax.set(
        title="Where the time goes (median ms; frames answered by the server, LOCAL: all)",
        xticks=list(x),
        xticklabels=labels,
        xlabel="configuration",
    )
    ax.legend()
    fig.tight_layout()
    fig.savefig(out_dir / "time_breakdown.png", dpi=150)
    plt.close(fig)


if __name__ == "__main__":
    raise SystemExit(main())
