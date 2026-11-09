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

# input.resize_mode in the manifest, from Edge Impulse's EI_CLASSIFIER_RESIZE_MODE
# (docs/MODELS.md). Same rules as the Edge Impulse SDK
# (edge-impulse-sdk/dsp/image/processing.cpp), bilinear like it.
RESIZE_MODES = ("none", "fit_shortest", "fit_longest", "squash")


def to_model_input(
    frame: np.ndarray, input_shape: tuple[int, ...], dtype: np.dtype, resize_mode: str
) -> np.ndarray:
    """An RGB uint8 frame [H, W, 3] as the model input [1, h, w, 3] in [0, 1].

    ``input_shape`` is the manifest's [1, h, w, 3]. A frame that already has
    the input size (Picamera2 scaled by the ISP) is not resized, whatever the
    mode: cropping or padding it would change nothing.
    """
    _, height, width, channels = input_shape
    if channels != 3:
        raise ValueError(f"model input has {channels} channels, expected RGB")
    if resize_mode not in RESIZE_MODES:
        raise ValueError(f"unknown resize_mode {resize_mode!r}; known: {list(RESIZE_MODES)}")
    if frame.ndim != 3 or frame.shape[2] != 3 or frame.dtype != np.uint8:
        raise ValueError(f"expected an RGB uint8 frame [H, W, 3], got {frame.dtype} {frame.shape}")

    if frame.shape[:2] != (height, width):
        from PIL import Image

        # PIL sizes and boxes are (x, y); numpy shapes are (rows, columns).
        bilinear = Image.Resampling.BILINEAR
        src_h, src_w = frame.shape[:2]
        image = Image.fromarray(frame)
        if resize_mode == "none":
            raise ValueError(
                f"resize_mode is none: the frame must be {width}x{height}, got {src_w}x{src_h}"
            )
        if resize_mode == "squash":
            image = image.resize((width, height), bilinear)
        elif resize_mode == "fit_shortest":
            # The largest centered box with the input aspect ratio, cropped and scaled in one step.
            if src_w * height > src_h * width:  # wider than the input: crop left and right
                box_w, box_h = src_h * width // height, src_h
            else:  # taller: crop top and bottom
                box_w, box_h = src_w, src_w * height // width
            left, top = (src_w - box_w) // 2, (src_h - box_h) // 2
            image = image.resize(
                (width, height), bilinear, box=(left, top, left + box_w, top + box_h)
            )
        else:  # fit_longest: scale to fit inside, then center on a black canvas
            if src_w * height > src_h * width:
                w, h = width, int(width * src_h / src_w)
            else:
                w, h = int(height * src_w / src_h), height
            canvas = Image.new("RGB", (width, height))
            scaled = image.resize((max(w, 1), max(h, 1)), bilinear)
            canvas.paste(scaled, ((width - w) // 2, (height - h) // 2))
            image = canvas
        frame = np.asarray(image)
    x = frame.astype(np.float32) / 255.0
    return x[np.newaxis].astype(dtype)


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
        # No default: a wrong resize mode silently changes what the model sees.
        self.resize_mode = manifest["input"].get("resize_mode")
        if self.resize_mode not in RESIZE_MODES:
            raise ValueError(
                f"{self.dir}: input.resize_mode is {self.resize_mode!r}, expected one of "
                f"{list(RESIZE_MODES)}: rebuild it with sciot-model build --overwrite"
            )
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
        """An image file as the model input (``to_model_input``)."""
        from PIL import Image

        return self.to_input(np.asarray(Image.open(path).convert("RGB")))

    def to_input(self, frame: np.ndarray) -> np.ndarray:
        """An RGB uint8 frame as this model's input (``to_model_input``)."""
        return to_model_input(frame, self.input_shape, self.input_dtype, self.resize_mode)

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
