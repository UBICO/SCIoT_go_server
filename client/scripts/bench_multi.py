"""Several devices on one server: does the offloading algorithm share it?

Method
------
One Go server (worker, recording, ``offload.algorithm``) and ``--devices``
clients with different device ids, started ``--stagger`` seconds apart (0 =
together: the case where the algorithm's reservations matter, phase 3.7).
Every client is released to the algorithm (the registration check pins it,
like any admin policy), or pinned to ``--pin`` (load chosen by hand), and
runs for ``--duration`` seconds.

Per device: frames per second, latency p50/p95 after ``--warmup`` seconds,
the share answered by the server, the policy changes and the last policy.
From the recording: every policy change with its reason, and the largest
load the algorithm saw reserved and not yet measured (``pending_erlangs``).

Client and server share the machine, so the devices compete for its CPU:
the numbers are for the algorithm's behaviour, not for capacity.

Example::

    uv run --project client python client/scripts/bench_multi.py \\
        --model-dir ~/sciot-models/yolo_small_320 --devices 3

Results: ``client/results/multi_bench/<time>/`` (or under $SCIOT_RESULTS_DIR):
results.json, results.md, one frames CSV per device and the recording.
"""

from __future__ import annotations

import argparse
import json
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from datetime import datetime
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from bench_common import (
    GO_REPO,
    RESULTS_DIR,
    log_tail,
    wait_http_ok,
)
from bench_split import (
    _floats,
    _p,
    client_config,
    go_config,
    http_json,
    read_frames,
    stop,
    wait_registered,
    wait_worker,
)

# ---------------------------------------------------------------------------
# Metrics (pure functions: tested without servers)
# ---------------------------------------------------------------------------


def policy_label(policy: dict) -> str:
    """LOCAL, or SPLIT k/w (w = window, "max" without one)."""
    if policy.get("mode") != "SPLIT":
        return "LOCAL"
    return f"SPLIT {policy.get('split_point')}/{policy.get('max_inflight') or 'max'}"


def pin_body(text: str) -> dict:
    """Split/window text ("6/2") -> the admin policy SPLIT at split point 6 with window 2."""
    split, sep, window = text.partition("/")
    if not sep:
        raise ValueError(f"--pin {text!r}: expected split/window, e.g. 6/2")
    return {"mode": "SPLIT", "split_point": int(split), "max_inflight": int(window)}


def device_metrics(frames: list[dict], warmup_s: float, end_s: float) -> dict:
    """What one device got between warmup_s and end_s (its frames CSV)."""
    kept = [r for r in frames if warmup_s <= float(r["t_s"]) <= end_s]
    seconds = end_s - warmup_s
    results = [r for r in kept if r["outcome"] == "result"]
    return {
        "frames": len(kept),
        "fps": len(kept) / seconds if seconds > 0 else 0.0,
        "latency_ms_p50": _p(_floats(kept, "latency_ms"), 50),
        "latency_ms_p95": _p(_floats(kept, "latency_ms"), 95),
        "result_fraction": len(results) / len(kept) if kept else 0.0,
    }


def policy_timeline(events: list[dict]) -> list[dict]:
    """Every policy change of the recording, in order, with its reason."""
    start = next((e["t_ms"] for e in events if "t_ms" in e), 0)
    return [
        {
            "t_s": (e["t_ms"] - start) / 1000,
            "device_id": e["device_id"],
            "policy": policy_label(e["policy"]),
            "version": e["policy"]["policy_version"],
            "reason": e.get("reason", ""),
        }
        for e in events
        if e.get("kind") == "policy"
    ]


def max_pending(events: list[dict]) -> float:
    """The largest load reserved by other devices and not yet measured."""
    return max(
        (e.get("pending_erlangs", 0.0) for e in events if e.get("kind") == "decision"),
        default=0.0,
    )


def format_table(per_device: dict[str, dict], timeline: list[dict]) -> str:
    lines = [
        "| Device | Frames/s | Latency p50 (ms) | Latency p95 (ms) | Server results | Changes | Last policy |",
        "|---|---|---|---|---|---|---|",
    ]
    fmt = lambda v: "—" if v is None else f"{v:.1f}"
    for device_id, m in per_device.items():
        mine = [c for c in timeline if c["device_id"] == device_id]
        changes = [c for c in mine if c["reason"] != "admin"]
        # Admin changes count for the last policy: with --pin it is the pin.
        last = mine[-1]["policy"] if mine else "LOCAL"
        lines.append(
            f"| {device_id} | {m['fps']:.1f} | {fmt(m['latency_ms_p50'])} | "
            f"{fmt(m['latency_ms_p95'])} | {m['result_fraction']:.0%} | {len(changes)} | {last} |"
        )
    lines += ["", "| t (s) | Device | Policy | Reason |", "|---|---|---|---|"]
    lines += [
        f"| {c['t_s']:.1f} | {c['device_id']} | {c['policy']} v{c['version']} | {c['reason']} |"
        for c in timeline
        if c["reason"] != "admin"
    ]
    return "\n".join(lines)


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def parse_args(argv: list[str] | None) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument("--model-dir", type=Path, required=True)
    p.add_argument("--devices", type=int, default=2)
    p.add_argument("--stagger", type=float, default=0.0, help="seconds between client starts")
    p.add_argument("--duration", type=float, default=60.0, help="seconds after the last start")
    p.add_argument("--warmup", type=float, default=20.0, help="seconds not measured, per device")
    p.add_argument("--algorithm", default="latency", help="offload.algorithm of the server")
    p.add_argument(
        "--pin",
        metavar="SPLIT/WINDOW",
        help="keep every device at this policy (e.g. 6/2) instead of releasing it: "
        "load chosen by hand, the algorithm is not asked",
    )
    p.add_argument("--go-repo", type=Path, default=GO_REPO)
    p.add_argument("--go-bin", type=Path, help="prebuilt Go server binary")
    p.add_argument("--python", default=sys.executable, help="interpreter with the client")
    p.add_argument("--slots", type=int, default=2, help="worker slots")
    p.add_argument("--threads", type=int, default=2, help="TFLite threads per slot")
    p.add_argument("--heartbeat-ms", type=int, default=1000)
    p.add_argument("--go-port", type=int, default=18390)
    p.add_argument("--worker-port", type=int, default=50390)
    p.add_argument("--out", type=Path, help="results directory")
    args = p.parse_args(argv)
    if args.devices < 1:
        p.error("--devices must be >= 1")
    args.model_dir = args.model_dir.expanduser().resolve()
    if not (args.model_dir / "manifest.json").is_file():
        p.error(f"{args.model_dir} has no manifest.json: build it with sciot-model build")
    args.go_repo = args.go_repo.expanduser().resolve()
    if args.pin:
        try:
            args.pin_body = pin_body(args.pin)
        except ValueError as exc:
            p.error(str(exc))
    return args


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    out_dir = args.out or RESULTS_DIR / "multi_bench" / datetime.now().strftime("%Y%m%d-%H%M%S")
    out_dir.mkdir(parents=True, exist_ok=True)
    server_url = f"http://127.0.0.1:{args.go_port}"
    ids = [f"multi_{i}" for i in range(args.devices)]
    server = None
    clients: list[subprocess.Popen] = []

    with tempfile.TemporaryDirectory(prefix="sciot-multi-bench-") as tmp_name:
        tmp = Path(tmp_name)
        record_dir, server_log = tmp / "record", tmp / "server.log"
        try:
            go_bin = args.go_bin
            if go_bin is None:
                go_bin = tmp / "sciot-server"
                subprocess.run(
                    ["go", "build", "-o", str(go_bin), "./cmd/server"], cwd=args.go_repo, check=True
                )
            cfg = go_config(
                args.model_dir,
                args.go_repo,
                args.worker_port,
                args.slots,
                args.threads,
                record_dir,
                args.heartbeat_ms,
            )
            cfg["offload"] = {"algorithm": args.algorithm}
            (tmp / "config.json").write_text(json.dumps(cfg))
            server = subprocess.Popen(
                [
                    str(go_bin),
                    "-config",
                    str(tmp / "config.json"),
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

            for i, device_id in enumerate(ids):
                if i:
                    time.sleep(args.stagger)
                config_path = tmp / f"{device_id}.yaml"
                config_path.write_text(
                    json.dumps(client_config(server_url, device_id, args.model_dir))
                )
                clients.append(
                    subprocess.Popen(
                        [
                            args.python,
                            "-m",
                            "sciot_client.http_go_client",
                            "--config",
                            str(config_path),
                            "--no-table",
                            "--frames-csv",
                            str(tmp / f"frames_{device_id}.csv"),
                        ],
                        stdout=(tmp / f"client_{device_id}.log").open("w"),
                        stderr=subprocess.STDOUT,
                    )
                )
            for device_id, client in zip(ids, clients, strict=True):
                # The check pins the device (an admin policy): release it, or pin it
                # to --pin.
                wait_registered(server_url, device_id, 120, client)
                policy_url = f"{server_url}/api/v1/admin/devices/{device_id}/policy"
                if args.pin:
                    status, body = http_json("POST", policy_url, args.pin_body)
                    if status != 200:
                        raise RuntimeError(f"pin {device_id} to {args.pin}: HTTP {status} {body}")
                else:
                    http_json("DELETE", policy_url)
            print(f"{len(ids)} clients registered, running {args.duration:.0f} s")
            time.sleep(args.duration)
        except Exception:
            for device_id in ids:
                print(log_tail(tmp / f"client_{device_id}.log"))
            print(log_tail(server_log))
            raise
        finally:
            for client in clients:
                stop(client, signal.SIGINT)
            stop(server, signal.SIGINT)

        records = sorted(record_dir.glob("*.jsonl"))
        events = [json.loads(line) for line in records[-1].read_text().splitlines() if line.strip()]
        shutil.copy(records[-1], out_dir / "record.jsonl")
        per_device = {}
        for device_id in ids:
            path = tmp / f"frames_{device_id}.csv"
            shutil.copy(path, out_dir / path.name)
            frames = read_frames(path)
            end = max((float(r["t_s"]) for r in frames), default=0.0)
            per_device[device_id] = device_metrics(frames, args.warmup, end)

    timeline = policy_timeline(events)
    results = {
        "args": {k: str(v) for k, v in vars(args).items()},
        "devices": per_device,
        "policy_changes": timeline,
        "max_pending_erlangs": max_pending(events),
    }
    (out_dir / "results.json").write_text(json.dumps(results, indent=2))
    table = format_table(per_device, timeline)
    (out_dir / "results.md").write_text(table + "\n")
    print(table)
    print(f"\nlargest reserved load not yet measured: {results['max_pending_erlangs']:.2f} erlang")
    print(f"results in {out_dir}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
