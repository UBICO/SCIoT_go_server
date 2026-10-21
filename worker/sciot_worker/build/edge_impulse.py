"""Read the post-processing parameters from an Edge Impulse C++ library export.

Studio → Deployment → "C++ library (Linux)" produces a ZIP with the whole SDK;
only three files are read:

- model-parameters/model_metadata.h: architecture of the last layer, input
  size, detection threshold, max detections, quantization and the data types
  of the model input and output;
- model-parameters/model_variables.h: labels in order, NMS IoU;
- tflite-model/*.tflite: the whole model as Edge Impulse runs it, used to
  check that the SavedModel export is the same model.

Only a few macros are read, by exact name. If Edge Impulse renames one, the
build stops with a clear error instead of guessing: see docs/MODELS.md.
"""

from __future__ import annotations

import re
import zipfile
from dataclasses import dataclass
from pathlib import Path

METADATA = "model-parameters/model_metadata.h"
VARIABLES = "model-parameters/model_variables.h"

# Last layer → SCIoT decoder. A last layer not listed here is a family
# without a decoder: adding one means writing it in the worker, the clients
# and the ESP32 (docs/MODELS.md, "Adding a decoder family").
DECODERS = {
    "EI_CLASSIFIER_LAST_LAYER_YOLO_PRO": "yolo_pro",
    "EI_CLASSIFIER_LAST_LAYER_FOMO": "fomo",
}


# Edge Impulse data type macros → numpy dtype names. They mirror TFLite's
# TfLiteType; a quantized model may use any of them, not only int8.
DATATYPES = {
    "EI_CLASSIFIER_DATATYPE_FLOAT32": "float32",
    "EI_CLASSIFIER_DATATYPE_INT8": "int8",
    "EI_CLASSIFIER_DATATYPE_UINT8": "uint8",
}


# The most detections Edge Impulse returns: EI_CLASSIFIER_OBJECT_DETECTION_KEEP_TOPK
# in the SDK (classifier/ei_nms.h), not a model parameter. Not to be confused
# with EI_CLASSIFIER_OBJECT_DETECTION_COUNT in model_metadata.h, which is the
# guaranteed MINIMUM number of results (the SDK pads with zero-score entries).
MAX_DETECTIONS = 200


class ExportError(RuntimeError):
    """The C++ export is missing something or uses an unsupported model."""


@dataclass(frozen=True)
class CppExport:
    project_id: int
    deploy_version: int
    studio_version: str  # "1.94.5"
    last_layer: str  # the macro name, e.g. EI_CLASSIFIER_LAST_LAYER_YOLO_PRO
    input_width: int
    input_height: int
    quantized: bool  # EI_CLASSIFIER_QUANTIZATION_ENABLED
    input_dtype: str  # "float32", "int8", "uint8", ...
    output_dtype: str
    labels: list[str]
    score_threshold: float
    iou_threshold: float | None  # None for decoders without NMS (FOMO)
    max_detections: int
    full_model: bytes  # tflite-model/*.tflite

    @property
    def decoder(self) -> str:
        try:
            return DECODERS[self.last_layer]
        except KeyError:
            supported = ", ".join(sorted(DECODERS))
            raise ExportError(
                f"unsupported last layer {self.last_layer}; supported: {supported}"
            ) from None

    def postprocess(self) -> dict:
        """The manifest's "postprocess" section."""
        section = {
            "decoder": self.decoder,
            "labels": list(self.labels),
            "score_threshold": self.score_threshold,
            "max_detections": self.max_detections,
        }
        if self.iou_threshold is not None:
            section["iou_threshold"] = self.iou_threshold
        return section


# ---------------------------------------------------------------------------
# Header parsing
# ---------------------------------------------------------------------------


def define(header: str, name: str) -> str:
    """The value of `#define NAME value`, as text. ExportError if missing."""
    # [ \t]+ and not \s+: a macro without a value must not take the next line.
    # The whole name is followed by blanks, so EI_..._FFT_3 never matches _FFT_32.
    found = re.findall(rf"^#define[ \t]+{re.escape(name)}[ \t]+(.+?)[ \t]*$", header, re.MULTILINE)
    if not found:
        raise ExportError(f"the header has no {name}")
    values = []
    for value in found:
        if not value.startswith('"'):  # a "//" inside a quoted string is not a comment
            value = re.sub(r"[ \t]*//.*$", "", value)
        values.append(value)
    # Edge Impulse redefines some macros inside #if blocks (e.g.
    # EI_CLASSIFIER_INFERENCING_ENGINE): without evaluating the #if we cannot
    # know which value applies, so a macro with different values is refused.
    if len(set(values)) > 1:
        raise ExportError(
            f"{name} is defined {len(values)} times with different values "
            f"({', '.join(values)}): cannot tell which one applies"
        )
    return values[0]


def labels(variables: str) -> list[str]:
    """The labels from ei_classifier_inferencing_categories_<id>[] = { "a", "b" };"""
    array = re.search(
        r"ei_classifier_inferencing_categories_\w*\[\]\s*=\s*\{(.*?)\};", variables, re.DOTALL
    )
    if array is None:
        raise ExportError("model_variables.h has no ei_classifier_inferencing_categories array")
    found = re.findall(r'"([^"]*)"', array.group(1))
    if not found:
        raise ExportError("the labels array in model_variables.h is empty")
    return found


def nms_iou(variables: str) -> float | None:
    """The NMS IoU threshold of ei_object_detection_nms_<id> = { conf, iou }; None if absent."""
    match = re.search(r"([0-9.]+)f?\s*,?\s*/\*\s*NMS IOU threshold\s*\*/", variables)
    if match is None:
        return None
    try:
        return float(match.group(1))
    except ValueError:
        raise ExportError(f"NMS IOU threshold is not a number: {match.group(1)!r}") from None


# ---------------------------------------------------------------------------
# Reading the export
# ---------------------------------------------------------------------------


def _read_files(path: Path) -> tuple[str, str, bytes]:
    """(model_metadata.h, model_variables.h, tflite) from a ZIP or a directory."""
    path = path.expanduser()
    if path.is_dir():
        tflites = sorted((path / "tflite-model").glob("*.tflite"))
        files = {METADATA: path / METADATA, VARIABLES: path / VARIABLES}
        missing = [name for name, p in files.items() if not p.is_file()]
        if missing or len(tflites) != 1:
            raise ExportError(
                f"{path}: not an Edge Impulse C++ export "
                f"(missing {missing or ['one tflite-model/*.tflite']})"
            )
        return (files[METADATA].read_text(), files[VARIABLES].read_text(), tflites[0].read_bytes())

    if path.suffix.lower() != ".zip" or not path.is_file():
        raise ExportError(f"expected the C++ export as a ZIP or a directory, got {path}")
    with zipfile.ZipFile(path) as zf:
        # The ZIP may or may not have a top-level folder: match by suffix.
        names = zf.namelist()

        def one(suffix: str) -> str:
            found = [n for n in names if n.endswith(suffix)]
            if len(found) != 1:
                raise ExportError(f"{path.name}: expected one {suffix}, found {len(found)}")
            return found[0]

        tflite = [n for n in names if "tflite-model/" in n and n.endswith(".tflite")]
        if len(tflite) != 1:
            raise ExportError(
                f"{path.name}: expected one tflite-model/*.tflite, found {len(tflite)}"
            )
        return (
            zf.read(one(METADATA)).decode(),
            zf.read(one(VARIABLES)).decode(),
            zf.read(tflite[0]),
        )


def read_cpp_export(path: Path) -> CppExport:
    metadata, variables, full_model = _read_files(path)

    def number(name: str) -> float:
        value = define(metadata, name)
        try:
            return float(value.rstrip("f"))
        except ValueError:
            raise ExportError(f"{name} is not a number: {value!r}") from None

    def datatype(name: str) -> str:
        value = define(metadata, name)
        if value not in DATATYPES:
            raise ExportError(
                f"{name} is {value}: unknown data type (known: {', '.join(DATATYPES)})"
            )
        return DATATYPES[value]

    studio = ".".join(
        define(metadata, f"EI_STUDIO_VERSION_{part}") for part in ("MAJOR", "MINOR", "PATCH")
    )
    export = CppExport(
        project_id=int(number("EI_CLASSIFIER_PROJECT_ID")),
        deploy_version=int(number("EI_CLASSIFIER_PROJECT_DEPLOY_VERSION")),
        studio_version=studio,
        last_layer=define(metadata, "EI_CLASSIFIER_OBJECT_DETECTION_LAST_LAYER"),
        input_width=int(number("EI_CLASSIFIER_INPUT_WIDTH")),
        input_height=int(number("EI_CLASSIFIER_INPUT_HEIGHT")),
        quantized=int(number("EI_CLASSIFIER_QUANTIZATION_ENABLED")) != 0,
        input_dtype=datatype("EI_CLASSIFIER_TFLITE_INPUT_DATATYPE"),
        output_dtype=datatype("EI_CLASSIFIER_TFLITE_OUTPUT_DATATYPE"),
        labels=labels(variables),
        score_threshold=number("EI_CLASSIFIER_OBJECT_DETECTION_THRESHOLD"),
        iou_threshold=nms_iou(variables),
        max_detections=MAX_DETECTIONS,
        full_model=full_model,
    )
    if int(number("EI_CLASSIFIER_LABEL_COUNT")) != len(export.labels):
        raise ExportError(
            f"EI_CLASSIFIER_LABEL_COUNT is {int(number('EI_CLASSIFIER_LABEL_COUNT'))} "
            f"but {len(export.labels)} labels were found"
        )
    _ = export.decoder  # an unsupported family fails here, before the long split
    return export
