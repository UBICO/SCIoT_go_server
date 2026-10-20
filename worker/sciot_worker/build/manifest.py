"""Manifest v2 and the model directory.

A model directory holds the segments and manifest.json. The Go server reads
the manifest at startup (input, segment count, post-processing) and checks
model_hash against the files; devices receive the post-processing at
registration. Format: docs/MODELS.md.

Why model_hash is written here although the server recomputes it: a
directory with segments from two different builds would otherwise be served
as a valid model. Regenerating a model, even from the same export, with a
different TensorFlow version produces different bytes, hence a different
hash: devices must use the files of the SAME build.
"""

from __future__ import annotations

import hashlib
import json
import shutil
import tempfile
from collections.abc import Sequence
from dataclasses import dataclass
from pathlib import Path

import tensorflow as tf

from ..models import model_hash
from .split import Boundary, Errors, Segment

FORMAT_VERSION = 2
MANIFEST = "manifest.json"
PREFIX = "submodel"


@dataclass(frozen=True)
class Source:
    """Where a build comes from: enough to rebuild it, without local paths."""

    savedmodel_file: str  # file name only: a full path would leak the builder's machine
    savedmodel_sha256: str
    ei_project_id: int
    ei_deploy_version: int
    ei_studio_version: str


@dataclass(frozen=True)
class Verification:
    samples: int  # real images used (0 = only the random sample)
    atol: float
    rtol: float
    against_savedmodel: Errors
    against_edge_impulse: Errors


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as f:
        while chunk := f.read(1 << 20):
            digest.update(chunk)
    return digest.hexdigest()


def segment_file(index: int) -> str:
    return f"{PREFIX}_{index}.tflite"


def _is_model_file(path: Path) -> bool:
    """Files a model directory may contain, including the legacy sizes.json."""
    if not path.is_file():
        return False
    if path.name in (MANIFEST, "sizes.json"):
        return True
    stem = path.stem.removeprefix(f"{PREFIX}_")
    return path.suffix == ".tflite" and stem != path.stem and stem.isdigit()


def _tensor(shape: Sequence[int | None], dtype: str) -> dict:
    """Shapes are written with batch 1: the data plane always sends one sample."""
    return {"shape": [1 if i == 0 and d is None else d for i, d in enumerate(shape)], "dtype": dtype}


def build_manifest(*, segments: Sequence[Segment], boundaries: Sequence[Boundary],
                   postprocess: dict, source: Source, verification: Verification) -> dict:
    """The manifest, without model_hash (added once the files are written)."""
    # Key order is the order in the file (json keeps insertion order).
    # "quantization" is None for float32; a quantized build will describe it
    # there and add scale/zero_point to each quantized tensor of "segments".
    # Cuts keep only the tensor names: shapes and sizes are already in
    # "segments" (the output of segment i is the cut i), and the names are
    # what a quantized build needs to reuse the same cuts.
    def errors(e: Errors) -> dict:
        return {"max_abs_error": e.max_abs, "max_rel_error": e.max_rel}

    first, last = segments[0], segments[-1]
    return {
        "format_version": FORMAT_VERSION,
        "quantization": None,
        "tool": {"name": "sciot-model", "tensorflow": tf.__version__},
        "source": {
            "savedmodel": {"file": source.savedmodel_file, "sha256": source.savedmodel_sha256},
            "edge_impulse": {
                "project_id": source.ei_project_id,
                "deploy_version": source.ei_deploy_version,
                "studio_version": source.ei_studio_version,
            },
        },
        "input": _tensor(first.input_shape, first.input_dtype),
        "output": _tensor(last.output_shape, last.output_dtype),
        "cuts": [{"tensor": b.tensor_name, "operation": b.op_name, "operation_type": b.op_type}
                 for b in boundaries],
        "segments": [{
            "index": s.index,
            "file": segment_file(s.index),
            "size_bytes": len(s.content),
            "sha256": hashlib.sha256(s.content).hexdigest(),
            "input": _tensor(s.input_shape, s.input_dtype),
            "output": _tensor(s.output_shape, s.output_dtype),
        } for s in segments],
        "verification": {
            "samples": verification.samples,
            "atol": verification.atol,
            "rtol": verification.rtol,
            "savedmodel": errors(verification.against_savedmodel),
            "edge_impulse": errors(verification.against_edge_impulse),
        },
        "postprocess": postprocess,
    }


def write_model_dir(output: Path, segments: Sequence[Segment], manifest: dict,
                    overwrite: bool = False) -> dict:
    """Write the segments and the manifest (with model_hash) to output.

    Files are first written to a staging directory next to output, then moved
    in: a failed build never leaves a half-written model behind. Returns the
    manifest as written.
    """
    output = output.expanduser().resolve()
    if output.exists():
        existing = [p for p in output.iterdir()]
        if existing and not overwrite:
            raise FileExistsError(f"{output} is not empty ({len(existing)} files); pass --overwrite")
        # --overwrite replaces a model directory, nothing else: a wrong
        # --output must not delete unrelated files.
        foreign = [p.name for p in existing if not _is_model_file(p)]
        if foreign:
            raise FileExistsError(f"{output} contains files that are not part of a model "
                                  f"({', '.join(sorted(foreign)[:5])}): refusing to overwrite it")
    output.parent.mkdir(parents=True, exist_ok=True)

    with tempfile.TemporaryDirectory(prefix=f".{output.name}-", dir=output.parent) as tmp:
        stage = Path(tmp)
        for segment in segments:
            (stage / segment_file(segment.index)).write_bytes(segment.content)
        manifest = {**manifest, "model_hash": model_hash(stage)}
        (stage / MANIFEST).write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")

        if output.exists():
            shutil.rmtree(output)  # only reached with overwrite
        stage.rename(output)
        # TemporaryDirectory would remove `tmp` on exit: it no longer exists
        # after the rename, so recreate an empty one for it to clean up.
        stage.mkdir()
    return manifest
