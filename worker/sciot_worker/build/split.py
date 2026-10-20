"""Split a TensorFlow SavedModel into a chain of single-input TFLite segments.

Ported from scripts/split_saved_model.py of the Python repo and adapted:

- the cut search checks that the graph's operations are in topological order
  (it relies on it, and silently produced wrong cuts otherwise);
- verification runs on real samples when given, against one or more
  reference functions, and reports the relative error only on significant
  values (near zero it is meaningless);
- no file writing here: the caller writes the segments with the manifest.

Cuts are tensors through which exactly ONE activation is live: everything
before the cut feeds only that tensor, so every segment has one input and one
output, which is the hand-off supported by the clients.
"""

from __future__ import annotations

import math
import tempfile
import zipfile
from collections.abc import Callable, Iterator, Sequence
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path

import numpy as np
import tensorflow as tf
from tensorflow.python.framework.convert_to_constants import (  # type: ignore
    convert_variables_to_constants_v2,
)

DEFAULT_SIGNATURE = "serving_default"

# Cuts right after an activation are preferred: the tensor is the output of
# a "layer" in the usual sense.
PREFERRED_BOUNDARY_OPS = {
    "AvgPool", "Elu", "Gelu", "LeakyRelu", "MaxPool", "Relu", "Relu6",
    "Selu", "Sigmoid", "Softmax", "Tanh",
}
FALLBACK_BOUNDARY_OPS = {
    "Add", "AddV2", "AvgPool", "BiasAdd", "ConcatV2", "Conv2D",
    "DepthwiseConv2dNative", "MatMul", "MaxPool",
}
BOUNDARY_NAME_HINTS = ("activation", "gelu", "pool", "relu", "silu", "softmax", "swish")

# Values smaller than this (in absolute value) do not enter the relative
# error: 1e-6 vs 2e-6 is a 100% relative error but no difference at all.
SIGNIFICANT = 1e-3


class SplitError(RuntimeError):
    """The model cannot be split into SCIoT-compatible segments."""


@dataclass(frozen=True)
class FrozenModel:
    function: Callable  # concrete function: tensor -> structured output
    graph_def: object
    input_tensor: object
    output_tensor: object


@dataclass(frozen=True)
class Boundary:
    tensor_name: str
    op_name: str
    op_type: str
    op_index: int
    shape: tuple[int | None, ...]
    dtype: str


@dataclass(frozen=True)
class Segment:
    index: int
    content: bytes  # the .tflite file
    input_shape: list[int | None]
    input_dtype: str
    output_shape: list[int | None]
    output_dtype: str


@dataclass(frozen=True)
class Errors:
    """Difference between the segment chain and a reference."""

    max_abs: float
    max_rel: float  # only over |expected| >= SIGNIFICANT; 0 if there are none
    equivalent: bool  # every element within atol + rtol * |expected|


# ---------------------------------------------------------------------------
# Loading
# ---------------------------------------------------------------------------


def shape_list(tensor) -> tuple[int | None, ...]:
    shape = tensor.shape
    if shape.rank is None:
        return ()
    return tuple(None if v is None else int(v) for v in shape.as_list())


def bytes_per_sample(shape: Sequence[int | None], dtype_size: int) -> int | None:
    """Bytes of one sample (batch 1); None if a non-batch dimension is dynamic."""
    if not shape:
        return None
    dims = [1 if shape[0] is None else shape[0], *shape[1:]]
    if any(d is None for d in dims):
        return None
    return math.prod(int(d) for d in dims) * dtype_size


def _safe_extract_zip(archive: Path, destination: Path) -> None:
    destination = destination.resolve()
    with zipfile.ZipFile(archive) as zf:
        for member in zf.infolist():
            path = (destination / member.filename).resolve()
            if not path.is_relative_to(destination):
                raise SplitError(f"unsafe path in ZIP archive: {member.filename!r}")
        zf.extractall(destination)


def _locate_saved_model(directory: Path) -> Path:
    if (directory / "saved_model.pb").is_file():
        return directory
    matches = list(directory.rglob("saved_model.pb"))
    if not matches:
        raise SplitError(f"no saved_model.pb under {directory}: a TensorFlow SavedModel is required")
    if len(matches) > 1:
        raise SplitError(f"more than one SavedModel under {directory}: {matches}")
    return matches[0].parent


@contextmanager
def resolve_saved_model(source: Path) -> Iterator[Path]:
    """A SavedModel directory, its saved_model.pb, or a ZIP containing one."""
    source = source.expanduser().resolve()
    if not source.exists():
        raise SplitError(f"source does not exist: {source}")
    if source.is_dir():
        yield _locate_saved_model(source)
    elif source.suffix.lower() == ".zip":
        with tempfile.TemporaryDirectory(prefix="sciot-saved-model-") as tmp:
            _safe_extract_zip(source, Path(tmp))
            yield _locate_saved_model(Path(tmp))
    elif source.name == "saved_model.pb":
        yield source.parent
    else:
        raise SplitError(f"expected a SavedModel directory, saved_model.pb or ZIP, got {source}")


def load_frozen_model(model_dir: Path, signature: str = DEFAULT_SIGNATURE) -> FrozenModel:
    """Load one single-input/single-output signature and freeze its variables."""
    try:
        loaded = tf.saved_model.load(str(model_dir))
    except Exception as exc:
        raise SplitError(f"TensorFlow could not load {model_dir}: {exc}") from exc
    if signature not in loaded.signatures:
        available = ", ".join(sorted(loaded.signatures)) or "none"
        raise SplitError(f"signature {signature!r} not found; available: {available}")

    fn = loaded.signatures[signature]
    positional, keyword = fn.structured_input_signature
    if positional or len(keyword) != 1:
        raise SplitError(f"exactly one named model input is required, {signature!r} has "
                         f"{len(positional) + len(keyword)}")
    if not isinstance(fn.structured_outputs, dict) or len(fn.structured_outputs) != 1:
        raise SplitError(f"exactly one model output is required, {signature!r} has "
                         f"{len(tf.nest.flatten(fn.structured_outputs))}")

    frozen = convert_variables_to_constants_v2(fn)
    if len(frozen.inputs) != 1 or len(frozen.outputs) != 1:
        raise SplitError("freezing did not produce a single-input/single-output graph")
    return FrozenModel(
        function=frozen,
        graph_def=frozen.graph.as_graph_def(add_shapes=True),
        input_tensor=frozen.inputs[0],
        output_tensor=frozen.outputs[0],
    )


# ---------------------------------------------------------------------------
# Cuts
# ---------------------------------------------------------------------------


def check_topological_order(operations: Sequence) -> None:
    """Raise SplitError unless every op comes after the ops that produce its inputs.

    find_safe_boundaries computes "depends on the input" and the live tensors
    in ONE pass over the operations: it is correct only if each op's inputs
    were produced by ops seen earlier.
    """
    position = {op: i for i, op in enumerate(operations)}
    for op in operations:
        for tensor in op.inputs:
            producer = tensor.op
            produced_at = position.get(producer)
            if produced_at is not None and produced_at >= position[op]:
                raise SplitError(
                    f"graph operations are not in topological order: {op.name!r} "
                    f"reads {tensor.name!r} but comes before its producer {producer.name!r}"
                )


def _is_preferred(op) -> bool:
    if op.type in PREFERRED_BOUNDARY_OPS:
        return True
    if any(hint in op.name.lower() for hint in BOUNDARY_NAME_HINTS):
        return True
    # SiLU/Swish is usually x * sigmoid(x).
    return op.type == "Mul" and any(t.op.type == "Sigmoid" for t in op.inputs)


def find_safe_boundaries(model: FrozenModel) -> tuple[list[Boundary], list[Boundary]]:
    """Return (preferred, all valid) single-live-activation boundaries, in graph order."""
    operations = list(model.function.graph.get_operations())
    check_topological_order(operations)
    position = {op: i for i, op in enumerate(operations)}
    input_op = model.input_tensor.op

    depends_on_input = {}
    for op in operations:
        depends_on_input[op] = op is input_op or any(
            depends_on_input.get(t.op, False) for t in op.inputs
        )

    # last_consumer[t] = position of the last op reading t; a tensor is live
    # between its producer and its last consumer.
    end_events: dict[int, list] = {}
    last_consumer = {}
    for op in operations:
        if not depends_on_input[op]:
            continue
        for t in op.outputs:
            last = max((position[c] for c in t.consumers()), default=-1)
            last_consumer[t.ref()] = last
            if last >= 0:
                end_events.setdefault(last, []).append(t)

    live = {}
    preferred: list[Boundary] = []
    valid: list[Boundary] = []
    output_index = position[model.output_tensor.op]
    for index, op in enumerate(operations):
        if depends_on_input[op]:
            for t in op.outputs:
                if last_consumer.get(t.ref(), -1) > index:
                    live[t.ref()] = t
        # A tensor last read by this op does not cross a cut right after it.
        for t in end_events.get(index, ()):
            live.pop(t.ref(), None)

        if index <= position[input_op] or index >= output_index or len(live) != 1:
            continue
        tensor = next(iter(live.values()))
        if tensor.op is not op:
            continue
        shape = shape_list(tensor)
        if len(shape) < 2 or tensor.dtype == tf.resource:
            continue
        boundary = Boundary(tensor.name, op.name, op.type, index, shape, tensor.dtype.name)
        valid.append(boundary)
        if _is_preferred(op):
            preferred.append(boundary)
    return preferred, valid


def _dedup(boundaries: Sequence[Boundary]) -> list[Boundary]:
    seen, out = set(), []
    for b in sorted(boundaries, key=lambda b: b.op_index):
        if b.tensor_name not in seen:
            seen.add(b.tensor_name)
            out.append(b)
    return out


def _limit(boundaries: Sequence[Boundary], max_segments: int) -> list[Boundary]:
    """Keep max_segments-1 cuts evenly spread over the candidates (0 = all)."""
    boundaries = list(boundaries)
    if max_segments == 0 or len(boundaries) <= max_segments - 1:
        return boundaries
    wanted = max_segments - 1
    if wanted == 1:
        return [boundaries[len(boundaries) // 2]]
    indices: list[int] = []
    for p in np.linspace(0, len(boundaries) - 1, num=wanted):
        if (i := int(round(float(p)))) not in indices:
            indices.append(i)
    return [boundaries[i] for i in indices]


def choose_boundaries(model: FrozenModel, requested: Sequence[str] = (),
                      max_segments: int = 0) -> list[Boundary]:
    """The cuts to use: the requested tensors, or the automatic ones.

    requested is how a later quantized build reuses the cuts of the float32 one
    (same tensor names → same split points on device and server).
    """
    if max_segments == 1 or max_segments < 0:
        raise SplitError("max_segments must be 0 (unlimited) or at least 2")
    preferred, valid = find_safe_boundaries(model)
    valid = _dedup(valid)
    if requested:
        by_name = {b.tensor_name: b for b in valid} | {b.tensor_name.removesuffix(":0"): b for b in valid}
        missing = [n for n in requested if n not in by_name]
        if missing:
            raise SplitError("not safe single-tensor cuts: " + ", ".join(missing))
        selected = _dedup([by_name[n] for n in requested])
    else:
        selected = _dedup(preferred) or [b for b in valid if b.op_type in FALLBACK_BOUNDARY_OPS]
    selected = _limit(selected, max_segments)
    if not selected:
        raise SplitError("no single-tensor cut found: the graph may branch from input to output")
    return selected


# ---------------------------------------------------------------------------
# Conversion
# ---------------------------------------------------------------------------


def _segment_function(graph_def, input_name: str, output_name: str):
    """The frozen graph pruned between two tensors."""

    def import_graph():
        tf.compat.v1.import_graph_def(graph_def, name="")

    wrapped = tf.compat.v1.wrap_function(import_graph, [])
    try:
        feed = wrapped.graph.get_tensor_by_name(input_name)
        fetch = wrapped.graph.get_tensor_by_name(output_name)
    except KeyError as exc:
        raise SplitError(f"segment endpoint not in the frozen graph: {exc}") from exc
    return wrapped.prune(feed, fetch)


def _convert(function) -> bytes:
    """float32 TFLite, no optimizations: the same numbers as the source model."""
    module = tf.Module()
    module.segment = function
    converter = tf.lite.TFLiteConverter.from_concrete_functions([function], module)
    try:
        return converter.convert()
    except Exception as exc:
        raise SplitError(f"TFLite conversion failed: {exc}") from exc


def _io(content: bytes) -> tuple[dict, dict]:
    interpreter = tf.lite.Interpreter(model_content=content)
    interpreter.allocate_tensors()
    inputs, outputs = interpreter.get_input_details(), interpreter.get_output_details()
    if len(inputs) != 1 or len(outputs) != 1:
        raise SplitError(f"segment is not single-input/single-output: {len(inputs)}, {len(outputs)}")
    return inputs[0], outputs[0]


def _shape(detail: dict) -> list[int | None]:
    return [None if int(v) < 0 else int(v) for v in detail.get("shape_signature", detail["shape"])]


def convert_segments(model: FrozenModel, boundaries: Sequence[Boundary]) -> list[Segment]:
    endpoints = [model.input_tensor.name, *(b.tensor_name for b in boundaries), model.output_tensor.name]
    segments = []
    for i, (start, end) in enumerate(zip(endpoints, endpoints[1:])):
        content = _convert(_segment_function(model.graph_def, start, end))
        inp, out = _io(content)
        segments.append(Segment(
            index=i, content=content,
            input_shape=_shape(inp), input_dtype=np.dtype(inp["dtype"]).name,
            output_shape=_shape(out), output_dtype=np.dtype(out["dtype"]).name,
        ))
    return segments


# ---------------------------------------------------------------------------
# Verification
# ---------------------------------------------------------------------------


def run_tflite(content: bytes, value: np.ndarray) -> np.ndarray:
    interpreter = tf.lite.Interpreter(model_content=content)
    detail = interpreter.get_input_details()[0]
    if tuple(int(d) for d in detail["shape"]) != value.shape:
        interpreter.resize_tensor_input(detail["index"], value.shape, strict=False)
    interpreter.allocate_tensors()
    detail = interpreter.get_input_details()[0]
    interpreter.set_tensor(detail["index"], np.asarray(value, dtype=detail["dtype"]))
    interpreter.invoke()
    return interpreter.get_tensor(interpreter.get_output_details()[0]["index"])


def run_chain(segments: Sequence[Segment], value: np.ndarray) -> np.ndarray:
    for segment in segments:
        value = run_tflite(segment.content, value)
    return value


def compare(expected: np.ndarray, actual: np.ndarray, atol: float, rtol: float) -> Errors:
    """Errors of actual against expected, and whether they are equivalent."""
    if expected.shape != actual.shape:
        raise SplitError(f"shape mismatch: expected {expected.shape}, got {actual.shape}")
    expected = np.asarray(expected, dtype=np.float64)
    actual = np.asarray(actual, dtype=np.float64)
    if expected.size == 0:
        return Errors(0.0, 0.0, True)

    diff = np.abs(expected - actual)
    significant = np.abs(expected) >= SIGNIFICANT
    max_rel = float((diff[significant] / np.abs(expected[significant])).max()) if significant.any() else 0.0
    # Per element, not from the two maxima: they can come from different elements.
    equivalent = bool(np.allclose(actual, expected, atol=atol, rtol=rtol))
    return Errors(float(diff.max()), max_rel, equivalent)


def verify(segments: Sequence[Segment], reference: Callable[[np.ndarray], np.ndarray],
           samples: Sequence[np.ndarray], atol: float, rtol: float) -> Errors:
    """Worst errors of the chain against reference over all samples."""
    worst = Errors(0.0, 0.0, True)
    for sample in samples:
        e = compare(reference(sample), run_chain(segments, sample), atol, rtol)
        worst = Errors(max(worst.max_abs, e.max_abs), max(worst.max_rel, e.max_rel),
                       worst.equivalent and e.equivalent)
    return worst


def frozen_reference(model: FrozenModel) -> Callable[[np.ndarray], np.ndarray]:
    """The source model as a reference function."""
    def run(x: np.ndarray) -> np.ndarray:
        return np.asarray(tf.nest.flatten(model.function(tf.convert_to_tensor(x)))[0])
    return run


def random_sample(shape: Sequence[int], seed: int = 20260810) -> np.ndarray:
    """Uniform [0, 1) float32: always available, but weak for detection models."""
    return np.random.default_rng(seed).random(tuple(shape), dtype=np.float32)


def image_sample(path: Path, height: int, width: int) -> np.ndarray:
    """An image as model input: RGB, resized to height x width, scaled to [0, 1].

    Only for verification: chain and reference see the same tensor, so the
    resize mode does not matter here (the clients do their own preprocessing).
    """
    image = tf.io.decode_image(path.read_bytes(), channels=3, expand_animations=False)
    image = tf.image.resize(tf.cast(image, tf.float32) / 255.0, (height, width))
    return image.numpy()[np.newaxis].astype(np.float32)
