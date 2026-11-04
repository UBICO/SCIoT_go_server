"""The model on the device: segments from manifest.json, input, execution.

A model directory is built by ``sciot-model build`` (docs/MODELS.md): the
segments, in execution order, and their input/output come from its
``manifest.json``, the same file the server reads. The device must use the
SAME files the server serves, or the model_hash does not match.

Same rules as the worker (worker/sciot_worker/models.py), in a separate copy:
the client must not depend on the worker. ``testdata/model_hash`` keeps the
copies (Go registry, worker, client) computing the same hash.
"""

from __future__ import annotations

import hashlib
import json
import time
from pathlib import Path

import numpy as np

MANIFEST_VERSION = 2


def model_hash(model_dir: str | Path) -> str:
    """MD5 of all *.tflite files, concatenated in NAME order.

    Not the execution order: "submodel_10.tflite" comes before
    "submodel_2.tflite". Same rule as the Go registry and the worker.
    """
    paths = sorted(Path(model_dir).glob("*.tflite"))
    if not paths:
        raise FileNotFoundError(f"no .tflite files in {model_dir}")
    digest = hashlib.md5()
    for path in paths:
        with path.open("rb") as f:
            while chunk := f.read(1 << 20):
                digest.update(chunk)
    return digest.hexdigest()


class Model:
    """The segments of one model directory, loaded once.

    ``run(x, first, last)`` runs segments first..last on x: the prefix
    (0..k) of every frame and the completion (k+1..last) of a SPLIT frame the
    server did not take use the same code.
    """

    def __init__(self, model_dir: str | Path, num_threads: int | None = None):
        self.dir = Path(model_dir).expanduser().resolve()
        manifest = json.loads((self.dir / "manifest.json").read_text(encoding="utf-8"))
        if manifest.get("format_version") != MANIFEST_VERSION:
            raise ValueError(
                f"{self.dir}: manifest format_version {manifest.get('format_version')!r}, "
                f"expected {MANIFEST_VERSION}: rebuild it with sciot-model build"
            )
        segments = sorted(manifest["segments"], key=lambda s: s["index"])
        if [s["index"] for s in segments] != list(range(len(segments))):
            raise ValueError(f"{self.dir}: segment indexes are not 0..{len(segments) - 1}")
        self.input_shape = tuple(manifest["input"]["shape"])
        self.input_dtype = np.dtype(manifest["input"]["dtype"])
        self.last_layer = len(segments) - 1

        # Imported here: TensorFlow takes seconds to load, and the tests of the
        # modules that only need the manifest do not need it.
        import tensorflow as tf

        self._segments = []
        for s in segments:
            interpreter = tf.lite.Interpreter(
                model_path=str(self.dir / s["file"]), num_threads=num_threads
            )
            interpreter.allocate_tensors()
            self._segments.append(
                (
                    interpreter,
                    interpreter.get_input_details()[0],
                    interpreter.get_output_details()[0],
                )
            )

    def load_input(self, path: str | Path) -> np.ndarray:
        """An image as the model input: RGB, resized, float32 in [0, 1], batch 1."""
        from PIL import Image

        _, height, width, channels = self.input_shape
        if channels != 3:
            raise ValueError(f"{self.dir}: input has {channels} channels, expected RGB")
        # PIL sizes are (width, height).
        img = Image.open(path).convert("RGB").resize((width, height))
        x = np.asarray(img, dtype=np.float32) / 255.0
        return x[np.newaxis].astype(self.input_dtype)

    def run(self, x: np.ndarray, first: int, last: int) -> tuple[np.ndarray, list[float]]:
        """Run segments first..last on x; (output, seconds of each segment).

        The seconds are the interpreter invocations only (perf_counter).
        first = last + 1 runs nothing: x itself (split point -1).
        """
        if not 0 <= first <= last + 1 or last > self.last_layer:
            raise ValueError(f"segments {first}..{last} outside 0..{self.last_layer}")
        seconds = []
        for interpreter, inp, out in self._segments[first : last + 1]:
            interpreter.set_tensor(inp["index"], x.astype(inp["dtype"], copy=False))
            t0 = time.perf_counter()
            interpreter.invoke()
            seconds.append(time.perf_counter() - t0)
            # get_tensor returns a copy: x stays valid after the next invoke().
            x = interpreter.get_tensor(out["index"])
        return x, seconds
