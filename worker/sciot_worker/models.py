"""Models: segment files, model hash, interpreters and chain execution."""

from __future__ import annotations

import hashlib
import re
import time
from dataclasses import dataclass
from pathlib import Path

import numpy as np
import tensorflow as tf

_SEGMENT_INDEX = re.compile(r"_(\d+)\.tflite$")


def segment_paths(model_dir: str | Path) -> list[Path]:
    """The *.tflite segments in EXECUTION order: by numeric index (0, 1, ..., 10)."""
    paths = list(Path(model_dir).glob("*.tflite"))
    if not paths:
        raise FileNotFoundError(f"no .tflite files in {model_dir}")
    for path in paths:
        if not _SEGMENT_INDEX.search(path.name):
            raise ValueError(f"{path.name}: expected a name ending in _<index>.tflite")
    return sorted(paths, key=lambda p: int(_SEGMENT_INDEX.search(p.name).group(1)))


def model_hash(model_dir: str | Path) -> str:
    """MD5 of all *.tflite files, concatenated in NAME order.

    Must match the Go registry (internal/registry) and the clients, which all
    sort by file NAME: "submodel_10.tflite" comes before "submodel_2.tflite".
    That is NOT the execution order of segment_paths (numeric): using it here
    would produce a different hash for any model with 10 or more segments.
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


@dataclass(frozen=True)
class SegmentSpec:
    """Input/output of one segment, as reported by Describe."""

    index: int
    input_shape: tuple[int, ...]
    input_dtype: np.dtype
    output_shape: tuple[int, ...]
    output_dtype: np.dtype

    @property
    def input_bytes(self) -> int:
        return int(np.prod(self.input_shape)) * self.input_dtype.itemsize

    @property
    def output_bytes(self) -> int:
        return int(np.prod(self.output_shape)) * self.output_dtype.itemsize


class LoadedModel:
    """One set of interpreters for one model. Used by one request at a time."""

    def __init__(self, model_key: str, model_dir: str | Path, num_threads: int):
        self.model_key = model_key
        self.interpreters = []
        for path in segment_paths(model_dir):
            interpreter = tf.lite.Interpreter(model_path=str(path), num_threads=num_threads)
            interpreter.allocate_tensors()
            self.interpreters.append(interpreter)

    @property
    def last_segment(self) -> int:
        return len(self.interpreters) - 1

    def specs(self) -> list[SegmentSpec]:
        """Input and output of every segment, in execution order."""
        specs = []
        for index, interpreter in enumerate(self.interpreters):
            inp = interpreter.get_input_details()[0]
            out = interpreter.get_output_details()[0]
            specs.append(
                SegmentSpec(
                    index=index,
                    input_shape=tuple(int(d) for d in inp["shape"]),
                    input_dtype=np.dtype(inp["dtype"]),
                    output_shape=tuple(int(d) for d in out["shape"]),
                    output_dtype=np.dtype(out["dtype"]),
                )
            )
        return specs

    def run_from(self, split_point: int, tensor: bytes) -> tuple[bytes, list[float]]:
        """Run segments split_point+1 .. last; return the output and per-segment ms.

        split_point is the last segment executed by the device (-1 = raw model
        input). Raises ValueError on an invalid split point or tensor size.
        """
        first = split_point + 1
        if not 0 <= first <= self.last_segment:
            raise ValueError(
                f"invalid split point {split_point}: expected -1..{self.last_segment - 1}"
            )
        detail = self.interpreters[first].get_input_details()[0]
        shape, dtype = tuple(detail["shape"]), np.dtype(detail["dtype"])
        expected = int(np.prod(shape)) * dtype.itemsize
        if len(tensor) != expected:
            raise ValueError(
                f"tensor is {len(tensor)} bytes, segment {first} expects {expected} "
                f"({dtype} {shape})"
            )
        x = np.frombuffer(tensor, dtype=dtype).reshape(shape)

        layer_ms: list[float] = []
        for i in range(first, self.last_segment + 1):
            interpreter = self.interpreters[i]
            interpreter.set_tensor(interpreter.get_input_details()[0]["index"], x)
            t0 = time.perf_counter()
            interpreter.invoke()
            layer_ms.append((time.perf_counter() - t0) * 1000)
            # get_tensor returns a copy: x stays valid after the next invoke().
            x = interpreter.get_tensor(interpreter.get_output_details()[0]["index"])
        return x.tobytes(), layer_ms
