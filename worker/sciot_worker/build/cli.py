"""sciot-model: build a SCIoT model directory from Edge Impulse exports.

    uv run --project worker sciot-model build \\
        ei-...-tensorflow-savedmodel-model.41.zip \\
        --ei-cpp-export sciot-...-cpp-linux-v8.zip \\
        --sample images/stop.png --sample images/bike.jpg \\
        --output ~/sciot-models/yolo_small_320

Steps, each one failing with a clear message before the next:
  1. read the C++ export (post-processing, labels; float32 only for now)
  2. load and freeze the SavedModel; check it matches the C++ export
  3. choose the cuts and convert the segments
  4. verify the chain against the SavedModel AND Edge Impulse's own .tflite
  5. write segments + manifest.json, print the model_hash
"""

from __future__ import annotations

import argparse
import sys
from collections.abc import Sequence
from pathlib import Path

from . import split
from .edge_impulse import CppExport, ExportError, read_cpp_export
from .manifest import Source, Verification, build_manifest, sha256_file, write_model_dir


class BuildError(RuntimeError):
    """The two exports do not describe the same supported model."""


def check_consistency(export: CppExport, model: split.FrozenModel) -> None:
    """The SavedModel and the C++ export must describe the same model."""
    input_shape = split.shape_list(model.input_tensor)
    output_shape = split.shape_list(model.output_tensor)
    # Without this the device would run a float model while Edge Impulse's
    # thresholds were tuned on the quantized one.
    if export.quantized or export.input_dtype != "float32" or export.output_dtype != "float32":
        raise BuildError(
            f"the C++ export is quantized (input {export.input_dtype}, output "
            f"{export.output_dtype}); sciot-model builds float32 only for now "
            "(docs/MODELS.md, Quantization)"
        )

    expected_input = (export.input_height, export.input_width)
    if len(input_shape) != 4 or input_shape[3] != 3 or tuple(input_shape[1:3]) != expected_input:
        raise BuildError(
            f"the SavedModel input is {list(input_shape)}, but the C++ export expects "
            f"[batch, {export.input_height}, {export.input_width}, 3]"
        )

    columns = {
        "yolo_pro": len(export.labels) + 4,  # xmin, ymin, xmax, ymax, one score per label
        "fomo": len(export.labels) + 1,  # background first
    }[export.decoder]
    if not output_shape or output_shape[-1] != columns:
        found = output_shape[-1] if output_shape else None
        raise BuildError(
            f"the SavedModel output has {found} columns (shape {list(output_shape)}), but "
            f"decoder {export.decoder} with {len(export.labels)} labels needs {columns}"
        )
    # The comparison of the outputs (same model, same weights) is not here:
    # it is the verification against export.full_model, after the split.


def tflite_reference(content: bytes):
    """A whole-model .tflite as a reference function."""
    return lambda x: split.run_tflite(content, x)


def build(args: argparse.Namespace) -> dict:
    print(f"Reading the C++ export: {args.ei_cpp_export}")
    export = read_cpp_export(args.ei_cpp_export)
    print(
        f"  project {export.project_id} deploy v{export.deploy_version}, "
        f"decoder {export.decoder}, labels {export.labels}"
    )

    with split.resolve_saved_model(args.savedmodel) as model_dir:
        print(f"Loading the SavedModel: {args.savedmodel}")
        model = split.load_frozen_model(model_dir, args.signature)
        check_consistency(export, model)

        boundaries = split.choose_boundaries(model, args.cut, args.max_segments)
        print(f"Selected {len(boundaries)} cuts ({len(boundaries) + 1} segments)")
        for b in boundaries:
            print(f"  {b.op_name} [{b.op_type}] {b.shape}")
        segments = split.convert_segments(model, boundaries)

        height, width = export.input_height, export.input_width
        samples = [split.random_sample((1, height, width, 3))]
        samples += [split.image_sample(p, height, width) for p in args.sample]
        if not args.sample:
            print(
                "WARNING: no --sample images: verification uses random noise only, "
                "which barely exercises a detection model's scores"
            )

        print(
            f"Verifying {len(samples)} sample(s) against the SavedModel and Edge Impulse's .tflite"
        )
        errors = {
            "SavedModel": split.verify(
                segments, split.frozen_reference(model), samples, args.atol, args.rtol
            ),
            "Edge Impulse .tflite": split.verify(
                segments, tflite_reference(export.full_model), samples, args.atol, args.rtol
            ),
        }
        for name, e in errors.items():
            print(f"  {name}: max_abs={e.max_abs:.3g} max_rel={e.max_rel:.3g}")
            if not e.equivalent:
                hint = (
                    " (are the two exports from the same model version?)"
                    if name.startswith("Edge")
                    else ""
                )
                raise BuildError(
                    f"segments differ from the {name} beyond atol={args.atol}, "
                    f"rtol={args.rtol}{hint}"
                )

    manifest = build_manifest(
        segments=segments,
        boundaries=boundaries,
        postprocess=export.postprocess(),
        resize_mode=export.resize_mode,
        source=Source(
            savedmodel_file=Path(args.savedmodel).name,
            savedmodel_sha256=sha256_file(Path(args.savedmodel))
            if Path(args.savedmodel).is_file()
            else "",
            ei_project_id=export.project_id,
            ei_deploy_version=export.deploy_version,
            ei_studio_version=export.studio_version,
        ),
        verification=Verification(
            samples=len(args.sample),
            atol=args.atol,
            rtol=args.rtol,
            against_savedmodel=errors["SavedModel"],
            against_edge_impulse=errors["Edge Impulse .tflite"],
        ),
    )
    written = write_model_dir(args.output, segments, manifest, overwrite=args.overwrite)
    print(f"Wrote {len(segments)} segments to {args.output}")
    print(f"model_hash {written['model_hash']}")
    print(
        "NOTE: devices must use the files of THIS build: a rebuild (e.g. with another "
        "TensorFlow version) produces a different model_hash"
    )
    return written


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="sciot-model", description=__doc__.split("\n")[0])
    sub = p.add_subparsers(dest="command", required=True)

    b = sub.add_parser("build", help="build a model directory from Edge Impulse exports")
    b.add_argument(
        "savedmodel",
        type=Path,
        help='"TensorFlow SavedModel" export: ZIP, directory or saved_model.pb',
    )
    b.add_argument(
        "--ei-cpp-export",
        type=Path,
        required=True,
        help='"C++ library" export: ZIP or extracted directory',
    )
    b.add_argument("--output", type=Path, required=True, help="model directory to create")
    b.add_argument(
        "--sample",
        type=Path,
        action="append",
        default=[],
        metavar="IMAGE",
        help="real image for the verification (repeatable; strongly recommended)",
    )
    b.add_argument(
        "--cut",
        action="append",
        default=[],
        metavar="TENSOR",
        help="use this tensor as a cut (repeatable); automatic cuts when omitted",
    )
    b.add_argument("--max-segments", type=int, default=0, help="0 = all automatic cuts")
    b.add_argument("--signature", default=split.DEFAULT_SIGNATURE)
    b.add_argument("--atol", type=float, default=1e-4)
    b.add_argument("--rtol", type=float, default=1e-4)
    b.add_argument("--overwrite", action="store_true", help="replace an existing model directory")
    return p


def main(argv: Sequence[str] | None = None) -> int:
    args = parser().parse_args(argv)
    try:
        build(args)
    except (split.SplitError, ExportError, BuildError, FileExistsError) as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
