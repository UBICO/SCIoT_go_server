"""Generate spike inputs and measure the in-process reference (variant 0).

    python gen_inputs.py --model-dir ../yolo-small --split 0 --out ../out --threads 2 --n 200

Writes, for split point k:
  input_k<k>.bin      random float32 activation (fixed seed) for segment k+1
  expected_k<k>.bin   output of the model from that activation, computed here
  baseline_k<k>.json  latency of the same chain run in this process: the pure
                      compute cost every other variant is compared with
"""

from __future__ import annotations

import argparse
import json
import statistics
import time
from pathlib import Path

import numpy as np
from segments import Slot, run_chain, segment_paths


def percentile(values: list[float], q: float) -> float:
    ordered = sorted(values)
    index = min(len(ordered) - 1, max(0, round(q / 100 * (len(ordered) - 1))))
    return ordered[index]


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("--model-dir", required=True)
    parser.add_argument("--split", type=int, required=True)
    parser.add_argument("--out", required=True)
    parser.add_argument("--threads", type=int, default=1)
    parser.add_argument("--warmup", type=int, default=20)
    parser.add_argument("--n", type=int, default=200)
    parser.add_argument("--seed", type=int, default=42)
    args = parser.parse_args(argv)

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    slot = Slot(0, segment_paths(args.model_dir), args.threads)

    shape, dtype = slot.input_spec(args.split + 1)
    rng = np.random.default_rng(args.seed)
    tensor = rng.random(shape, dtype=np.float32).astype(dtype).tobytes()

    expected, _ = run_chain(slot, args.split, tensor)
    (out / f"input_k{args.split}.bin").write_bytes(tensor)
    (out / f"expected_k{args.split}.bin").write_bytes(expected)

    for _ in range(args.warmup):
        run_chain(slot, args.split, tensor)
    latencies = []
    for _ in range(args.n):
        start = time.perf_counter()
        run_chain(slot, args.split, tensor)
        latencies.append((time.perf_counter() - start) * 1000)

    result = {
        "variant": "inprocess",
        "split": args.split,
        "threads": args.threads,
        "input_bytes": len(tensor),
        "output_bytes": len(expected),
        "n": args.n,
        "mean_ms": statistics.fmean(latencies),
        "p50_ms": percentile(latencies, 50),
        "p95_ms": percentile(latencies, 95),
    }
    (out / f"baseline_k{args.split}.json").write_text(json.dumps(result, indent=2))
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
