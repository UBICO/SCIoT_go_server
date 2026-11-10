"""Network benchmark: does the server's bandwidth estimate match the network?

The Go server measures each device's network on its stream (docs/RECORDING.md, "Network"): bandwidth from how long it takes to read a frame,
latency from WebSocket pings. This benchmark imposes a known upload bandwidth
and compares it with what the server estimated.

Method
------
One Go server (with its worker, ``-admin`` and recording) and ONE client in
SPLIT at ``--split``, which talks to the server through a proxy that caps the
upload (client → server) like one link. For every limit of ``--limits-mbps``
(0 = no limit), in order:

1. set the limit on the proxy;
2. wait ``--warmup`` seconds: the estimate is a moving average and the
   "not limiting" flag looks at the samples of the last 60 s;
3. measure for ``--measure`` seconds.

Per limit it reports the server's estimate (median over the heartbeats of the
window, from the recording), the throughput really carried by the proxy
(the client may not fill the link), the per-frame bandwidth of the valid
samples, how many samples were valid, the "not limiting" flag and the latency.

Expected: estimate ≈ imposed limit when the link is the bottleneck (a few %
below: WebSocket and frame headers); "not limiting" with no limit on
localhost. Choose ``--split`` so that a frame is many times 64 KiB (the
server's read size): the model input (-1) is a good default.

Example::

    uv run --project client python client/scripts/bench_network.py \\
        --model-dir ~/sciot-models/yolo_small_320

Results: ``client/results/network_bench/<time>/`` (or under
$SCIOT_RESULTS_DIR): results.json, results.md, frames.csv and the recording.
"""

from __future__ import annotations

import argparse
import json
import re
import shutil
import signal
import statistics
import subprocess
import sys
import tempfile
import time
from datetime import UTC, datetime, timedelta
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from bench_common import (
    GO_REPO,
    RESULTS_DIR,
    CountingProxy,
    log_tail,
    wait_http_ok,
)
from bench_split import (
    client_config,
    go_config,
    set_policy,
    stop,
    wait_first_frame,
    wait_registered,
    wait_worker,
)

# ---------------------------------------------------------------------------
# Metrics (pure functions: tested without servers)
# ---------------------------------------------------------------------------


def parse_ts(ts: str) -> datetime:
    """A recording timestamp (Go RFC 3339, up to 9 decimals) as UTC datetime.

    datetime.fromisoformat of Python 3.11 accepts at most 6 decimals.
    """
    ts = re.sub(r"(\.\d{6})\d+", r"\1", ts.replace("Z", "+00:00"))
    return datetime.fromisoformat(ts)


def read_events(record_dir: Path) -> tuple[list[dict], Path | None]:
    """All events of the latest recording."""
    files = sorted(record_dir.glob("*.jsonl"))
    if not files:
        return [], None
    lines = files[-1].read_text().splitlines()
    return [json.loads(line) for line in lines if line.strip()], files[-1]


def in_window(
    events: list[dict], kind: str, device_id: str, start: datetime, end: datetime
) -> list[dict]:
    return [
        e
        for e in events
        if e.get("kind") == kind
        and e.get("device_id") == device_id
        and start <= parse_ts(e["ts"]) < end
    ]


def _median(values: list[float]) -> float | None:
    return statistics.median(values) if values else None


def to_mbps(bytes_per_s: float | None) -> float | None:
    return None if bytes_per_s is None else bytes_per_s * 8 / 1e6


def network_metrics(heartbeats: list[dict], frames: list[dict]) -> dict:
    """What the server saw in one window: heartbeat events, and the frame and
    probe events (both are reception samples; a probe has "bytes" instead of
    "request_bytes")."""
    nets = [h["network"] for h in heartbeats if h.get("network")]
    estimates = [n["bandwidth_bps"] for n in nets if n.get("bandwidth_bps")]
    latencies = [n["latency_ms"] for n in nets if n.get("latency_ms") is not None]
    # The counters are cumulative: the window's samples are last minus first.
    valid = discarded = None
    if len(nets) >= 2:
        valid = nets[-1]["valid_samples"] - nets[0]["valid_samples"]
        discarded = nets[-1]["discarded_samples"] - nets[0]["discarded_samples"]
    per_frame = [
        f.get("request_bytes", f.get("bytes", 0)) / (f["receive_ms"] / 1000)
        for f in frames
        if f.get("receive_valid") and f.get("receive_ms", 0) > 0
    ]
    return {
        "heartbeats": len(nets),
        "estimate_mbps": to_mbps(_median(estimates)),
        "frame_valid_mbps": to_mbps(_median(per_frame)),
        "valid_samples": valid,
        "discarded_samples": discarded,
        "not_limiting": nets[-1]["not_limiting"] if nets else None,
        "latency_ms": _median(latencies),
        "receive_ms_p50": _median(
            [f["receive_ms"] for f in frames if f.get("receive_ms") is not None]
        ),
        "frames": len(frames),
    }


def format_table(phases: list[dict]) -> str:
    def cell(v: float | None, fmt: str = ".1f") -> str:
        return "—" if v is None else format(v, fmt)

    lines = [
        (
            "| Limit (Mbit/s) | Carried (Mbit/s) | Estimate (Mbit/s) | Estimate / limit | "
            "Valid samples (Mbit/s) | Valid / discarded | Not limiting | Latency (ms) | "
            "Receive p50 (ms) |"
        ),
        "|" + "---|" * 9,
    ]
    for p in phases:
        limit = p["limit_mbps"]
        ratio = None if not limit or p["estimate_mbps"] is None else p["estimate_mbps"] / limit
        counts = (
            "—"
            if p["valid_samples"] is None
            else f"{p['valid_samples']} / {p['discarded_samples']}"
        )
        lines.append(
            f"| {'none' if not limit else cell(limit)} | {cell(p['carried_mbps'])} | "
            f"{cell(p['estimate_mbps'])} | {cell(ratio, '.2f')} | "
            f"{cell(p['frame_valid_mbps'])} | {counts} | "
            f"{'—' if p['not_limiting'] is None else p['not_limiting']} | "
            f"{cell(p['latency_ms'], '.2f')} | {cell(p['receive_ms_p50'])} |"
        )
    return "\n".join(lines)


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
    p.add_argument(
        "--split",
        default="-1",
        help="split point of the client (use --split=-1 for negative values), or "
        "LOCAL: no frames, the server measures with probes",
    )
    p.add_argument(
        "--probe-interval-ms",
        type=int,
        help="server's stream.probe_interval_ms (default: the server's, 30000); "
        "lower it with --split LOCAL to get several probes per limit",
    )
    p.add_argument(
        "--limits-mbps",
        default="10,50,0",
        help="upload limits in Mbit/s, in order; 0 = no limit",
    )
    p.add_argument("--warmup", type=float, default=15.0, help="seconds after each limit change")
    p.add_argument("--measure", type=float, default=20.0, help="seconds measured per limit")
    p.add_argument("--slots", type=int, default=2, help="worker slots")
    p.add_argument("--threads", type=int, default=2, help="TFLite threads per slot")
    p.add_argument("--heartbeat-ms", type=int, default=1000)
    p.add_argument("--go-port", type=int, default=18290)
    p.add_argument("--proxy-port", type=int, default=18291)
    p.add_argument("--worker-port", type=int, default=50290)
    p.add_argument("--device-id", default="bench_network")
    p.add_argument("--out", type=Path, help="results directory")
    args = p.parse_args(argv)
    try:
        args.limits = [float(v) for v in args.limits_mbps.split(",")]
    except ValueError:
        p.error(f"--limits-mbps: not a list of numbers: {args.limits_mbps!r}")
    if any(v < 0 for v in args.limits):
        p.error("--limits-mbps: limits must be >= 0")
    args.model_dir = args.model_dir.expanduser().resolve()
    if not (args.model_dir / "manifest.json").is_file():
        p.error(f"{args.model_dir} has no manifest.json: build it with sciot-model build")
    args.go_repo = args.go_repo.expanduser().resolve()
    return args


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    total = len(args.limits) * (args.warmup + args.measure)
    print(f"{len(args.limits)} limits: about {total / 60:.1f} min")

    out_dir = args.out or RESULTS_DIR / "network_bench" / datetime.now().strftime("%Y%m%d-%H%M%S")
    out_dir.mkdir(parents=True, exist_ok=True)
    server_url = f"http://127.0.0.1:{args.go_port}"
    server = client = proxy = None
    windows: list[tuple[float, datetime, datetime, int]] = []

    with tempfile.TemporaryDirectory(prefix="sciot-network-bench-") as tmp_name:
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
            server_config = go_config(
                args.model_dir,
                args.go_repo,
                args.worker_port,
                args.slots,
                args.threads,
                record_dir,
                args.heartbeat_ms,
            )
            if args.probe_interval_ms is not None:
                server_config["stream"] = {"probe_interval_ms": args.probe_interval_ms}
            config_path.write_text(json.dumps(server_config))
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
            version = set_policy(server_url, args.device_id, args.split)
            wait_first_frame(frames_csv, version, 60, client)
            print(
                "client in LOCAL" if args.split == "LOCAL" else f"client in SPLIT at {args.split}"
            )

            for i, limit in enumerate(args.limits, 1):
                proxy.set_up_limit(limit * 1e6 / 8 if limit else None)
                time.sleep(args.warmup)
                proxy.reset()
                start = datetime.now(UTC)
                time.sleep(args.measure)
                up, _ = proxy.counts()
                end = datetime.now(UTC)
                windows.append((limit, start, end, up))
                label = f"{limit:g} Mbit/s" if limit else "no limit"
                print(f"[{i}/{len(args.limits)}] {label}: done")
        except BaseException:
            print(
                f"server log:\n{log_tail(server_log)}\nclient log:\n{log_tail(client_log)}",
                file=sys.stderr,
            )
            raise
        finally:
            stop(client, signal.SIGINT)
            stop(server, signal.SIGTERM)  # the recording gets its "end" line
            if proxy is not None:
                proxy.stop()
            for path in (frames_csv, server_log, client_log):
                if path.exists():
                    shutil.copy(path, out_dir / path.name)

        events, record_file = read_events(record_dir)
        if record_file is not None:
            shutil.copy(record_file, out_dir / "record.jsonl")

    phases = []
    for limit, start, end, up in windows:
        measured_s = (end - start) / timedelta(seconds=1)
        phases.append(
            {
                "limit_mbps": limit,
                "carried_mbps": up * 8 / 1e6 / measured_s,
                **network_metrics(
                    in_window(events, "heartbeat", args.device_id, start, end),
                    in_window(events, "frame", args.device_id, start, end)
                    + in_window(events, "probe", args.device_id, start, end),
                ),
            }
        )
    table = format_table(phases)
    results = {
        "settings": {k: str(v) if isinstance(v, Path) else v for k, v in vars(args).items()},
        "phases": phases,
    }
    (out_dir / "results.json").write_text(json.dumps(results, indent=2) + "\n")
    (out_dir / "results.md").write_text(table + "\n")
    print(table)
    print(f"\nresults in {out_dir}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
