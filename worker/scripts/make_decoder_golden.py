"""Write a REAL golden file for a decoder: the model output on an image.

    uv run --project worker python worker/scripts/make_decoder_golden.py \
        ~/sciot-models/yolo_small_320 image.jpg testdata/decoders/yolo_pro/real_stop

writes real_stop.bin (the model output, float32 little-endian) and
real_stop.json (parameters, shape, provenance, expected detections).

The expected detections are produced by the worker's decoder: the worker is
the reference for the other implementations (Python client, ESP32), which
must match it on real data too. The image itself is not stored.
"""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path

import numpy as np
import tensorflow as tf

from sciot_worker import decoders
from sciot_worker.models import LoadedModel


def model_input(path: Path, height: int, width: int) -> np.ndarray:
    """RGB, resized to height x width (bilinear, no crop), scaled to [0, 1]."""
    image = tf.io.decode_image(path.read_bytes(), channels=3, expand_animations=False)
    image = tf.image.resize(tf.cast(image, tf.float32) / 255.0, (height, width))
    return image.numpy()[np.newaxis].astype(np.float32)


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    p.add_argument("model_dir", type=Path, help="directory built by sciot-model build")
    p.add_argument("image", type=Path)
    p.add_argument("out", type=Path, help="output path without extension")
    p.add_argument("--rule", required=True, help="what this file checks, in one sentence")
    args = p.parse_args()

    manifest = json.loads((args.model_dir / "manifest.json").read_text())
    pp = manifest["postprocess"]
    _, height, width, _ = manifest["input"]["shape"]

    model = LoadedModel("golden", args.model_dir, num_threads=1)
    raw, _ = model.run_from(-1, model_input(args.image, height, width).tobytes())
    output = np.frombuffer(raw, dtype="<f4").reshape(manifest["output"]["shape"])

    params = decoders.Params(
        pp["decoder"],
        tuple(pp["labels"]),
        pp["score_threshold"],
        pp["max_detections"],
        pp.get("iou_threshold"),
    )
    found = decoders.decode(params, output)

    args.out.with_suffix(".bin").write_bytes(output.astype("<f4").tobytes())
    doc = {
        "rule": args.rule,
        "postprocess": pp,
        "columns": ["xmin", "ymin", "xmax", "ymax"] + [f"score:{label}" for label in pp["labels"]],
        "output": {
            "file": args.out.with_suffix(".bin").name,
            "shape": manifest["output"]["shape"],
            "dtype": "float32",
            "byteorder": "little",
            "sha256": hashlib.sha256(output.astype("<f4").tobytes()).hexdigest(),
        },
        "source": {
            "image": args.image.name,
            "model_hash": manifest["model_hash"],
            "preprocessing": "RGB, bilinear resize without crop, /255",
        },
        "detections": [
            {"label": d.label, "score": d.score, "box": {"x": d.x, "y": d.y, "w": d.w, "h": d.h}}
            for d in found
        ],
    }
    args.out.with_suffix(".json").write_text(json.dumps(doc, indent=2) + "\n")
    print(
        f"{args.out}: {len(found)} detections, "
        f"{int((output[..., 4:] >= pp['score_threshold']).sum())} rows above the threshold"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
