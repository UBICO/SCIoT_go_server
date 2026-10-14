"""Loading and running a chain of TFLite segments (shared by worker and baseline)."""

from __future__ import annotations

import re
import time
from pathlib import Path

import numpy as np
import tensorflow as tf


def segment_paths(model_dir: str | Path) -> list[Path]:
    """submodel_<i>.tflite files ordered by NUMERIC index (0, 1, ..., 10, 11)."""
    paths = list(Path(model_dir).glob("submodel_*.tflite"))
    if not paths:
        raise FileNotFoundError(f"no submodel_*.tflite in {model_dir}")
    return sorted(paths, key=lambda p: int(re.search(r"_(\d+)\.tflite$", p.name).group(1)))


class Slot:
    """One complete set of interpreters: can run one request at a time."""

    def __init__(self, index: int, paths: list[Path], num_threads: int):
        self.index = index
        self.interpreters = []
        for path in paths:
            interpreter = tf.lite.Interpreter(model_path=str(path), num_threads=num_threads)
            interpreter.allocate_tensors()
            self.interpreters.append(interpreter)

    def input_spec(self, segment: int) -> tuple[tuple[int, ...], np.dtype]:
        detail = self.interpreters[segment].get_input_details()[0]
        return tuple(detail["shape"]), np.dtype(detail["dtype"])

    @property
    def last_segment(self) -> int:
        return len(self.interpreters) - 1


def run_chain(slot: Slot, split_point: int, tensor: bytes) -> tuple[bytes, list[float]]:
    """Run segments split_point+1 .. last on `tensor`; return output and per-segment ms.

    split_point is the last segment executed by the device, so the first segment
    to run here is split_point + 1. Raises ValueError on an invalid split point
    or a tensor whose size does not match that segment's input.
    """
    # split_point = -1 means "the raw model input", like the Python server.
    first = split_point + 1
    if not 0 <= first <= slot.last_segment:
        raise ValueError(
            f"invalid split point {split_point}: expected -1..{slot.last_segment - 1}"
        )

    shape, dtype = slot.input_spec(first)
    expected = int(np.prod(shape))
    # Check the size BEFORE frombuffer/reshape so the error is explicit instead of
    # numpy's generic "buffer size must be a multiple of element size".
    if len(tensor) != expected * dtype.itemsize:
        raise ValueError(
            f"tensor size mismatch for segment {first}: got {len(tensor)} bytes, "
            f"expected {expected * dtype.itemsize} ({expected} x {dtype} = shape {shape})"
        )
    x = np.frombuffer(tensor, dtype=dtype).reshape(shape)

    layer_ms: list[float] = []
    for i in range(first, slot.last_segment + 1):
        interp = slot.interpreters[i]
        interp.set_tensor(interp.get_input_details()[0]["index"], x)
        t0 = time.perf_counter()
        interp.invoke()
        layer_ms.append((time.perf_counter() - t0) * 1000)
        # get_tensor returns a COPY, so x stays valid even if another request later
        # uses the same interpreter (it would not with tensor(), which is a view).
        x = interp.get_tensor(interp.get_output_details()[0]["index"])

    return x.tobytes(), layer_ms
