"""sciot-model build on a tiny detector-shaped model and a fake C++ export.

The model: image (None, 8, 8, 3) → three ReLU stages → (None, 32, 6), the
YOLO-Pro layout with 2 labels (4 box columns + 2 scores). The fake C++
export has hand-written headers in Edge Impulse's format and the whole model
as tflite-model/*.tflite, like the real export.
"""

from __future__ import annotations

import json
import zipfile
from pathlib import Path

import numpy as np
import pytest
import tensorflow as tf

from sciot_worker.build import cli, edge_impulse, manifest, split
from sciot_worker.models import model_hash


class TinyDetector(tf.Module):
    def __init__(self, scale: float = 2.0):
        super().__init__()
        self.scale = scale

    @tf.function(input_signature=[tf.TensorSpec([None, 8, 8, 3], tf.float32, name="images")])
    def serve(self, images):
        a = tf.nn.relu(images * self.scale, name="stem_relu")
        b = tf.nn.relu(a + 0.5, name="mid_relu")
        c = tf.nn.relu(b * 0.5 - 0.1, name="last_relu")
        return {"output_0": tf.reshape(c, [-1, 32, 6])}


def save(path: Path, scale: float = 2.0) -> Path:
    model = TinyDetector(scale)
    tf.saved_model.save(
        model, str(path), signatures={"serving_default": model.serve.get_concrete_function()}
    )
    return path


METADATA = """\
#define EI_CLASSIFIER_PROJECT_ID                 1011188
#define EI_CLASSIFIER_PROJECT_DEPLOY_VERSION     8
#define EI_CLASSIFIER_INPUT_WIDTH                8
#define EI_CLASSIFIER_INPUT_HEIGHT               8
#define EI_CLASSIFIER_LABEL_COUNT                2
#define EI_CLASSIFIER_OBJECT_DETECTION_LAST_LAYER  {last_layer}
#define EI_CLASSIFIER_OBJECT_DETECTION_COUNT       10
#define EI_CLASSIFIER_OBJECT_DETECTION_THRESHOLD   0.5
#define EI_CLASSIFIER_LOAD_FFT_32                0
#define EI_CLASSIFIER_LOAD_FFT_3                 1
#define EI_CLASSIFIER_QUANTIZATION_ENABLED          {quantized}
#define EI_CLASSIFIER_TFLITE_INPUT_DATATYPE      {dtype}
#define EI_CLASSIFIER_TFLITE_OUTPUT_DATATYPE     {dtype}
#define EI_STUDIO_VERSION_MAJOR             1
#define EI_STUDIO_VERSION_MINOR             94
#define EI_STUDIO_VERSION_PATCH             5
"""

VARIABLES = """\
const char* ei_classifier_inferencing_categories_1011188_12[] = { "bicycle", "stop sign" };
const ei_object_detection_nms_config_t ei_object_detection_nms_1011188_41 = {
    0.0f, /* NMS confidence threshold */
    0.2f  /* NMS IOU threshold */
};
"""


def fake_export(
    path: Path,
    saved_model: Path,
    *,
    last_layer="EI_CLASSIFIER_LAST_LAYER_YOLO_PRO",
    quantized=0,
    dtype="EI_CLASSIFIER_DATATYPE_FLOAT32",
) -> Path:
    (path / "model-parameters").mkdir(parents=True)
    (path / "tflite-model").mkdir()
    (path / edge_impulse.METADATA).write_text(
        METADATA.format(last_layer=last_layer, quantized=quantized, dtype=dtype)
    )
    (path / edge_impulse.VARIABLES).write_text(VARIABLES)
    full = tf.lite.TFLiteConverter.from_saved_model(str(saved_model)).convert()
    (path / "tflite-model" / "tflite_learn_1011188_41.tflite").write_bytes(full)
    return path


def zip_dir(directory: Path, archive: Path, top: str = "export/") -> Path:
    with zipfile.ZipFile(archive, "w") as zf:
        for p in directory.rglob("*"):
            if p.is_file():
                zf.write(p, top + str(p.relative_to(directory)))
    return archive


def sample_png(path: Path) -> Path:
    pixels = np.random.default_rng(1).integers(0, 256, (16, 12, 3), dtype=np.uint8)
    path.write_bytes(tf.io.encode_png(pixels).numpy())
    return path


@pytest.fixture(scope="module")
def tiny(tmp_path_factory):
    root = tmp_path_factory.mktemp("build")
    saved = save(root / "saved_model")
    return {"root": root, "saved": saved, "export": fake_export(root / "cpp", saved)}


def build_args(tiny, out: Path, *extra: str) -> list[str]:
    return [
        "build",
        str(tiny["saved"]),
        "--ei-cpp-export",
        str(tiny["export"]),
        "--output",
        str(out),
        *extra,
    ]


# --- split ------------------------------------------------------------------------------


def test_topological_order(tiny):
    model = split.load_frozen_model(tiny["saved"])
    ops = list(model.function.graph.get_operations())
    split.check_topological_order(ops)  # a real graph is fine
    with pytest.raises(split.SplitError):
        split.check_topological_order(list(reversed(ops)))


def test_compare():
    expected = np.array([[1.0, 2.0, 1e-6]], dtype=np.float32)
    e = split.compare(expected, expected.copy(), atol=1e-4, rtol=1e-4)
    assert (e.max_abs, e.max_rel, e.equivalent) == (0.0, 0.0, True)
    assert isinstance(e.max_abs, float) and isinstance(e.equivalent, bool)

    # 1e-6 vs 3e-6: 200 % relative, but not significant → not in max_rel.
    e = split.compare(expected, np.array([[1.0, 2.0, 3e-6]], dtype=np.float32), 1e-4, 1e-4)
    assert e.max_rel == 0.0 and e.equivalent

    e = split.compare(expected, np.array([[1.1, 2.0, 1e-6]], dtype=np.float32), 1e-4, 1e-4)
    assert e.max_rel == pytest.approx(0.1, rel=1e-5) and not e.equivalent

    with pytest.raises(split.SplitError):
        split.compare(expected, np.zeros((2, 3)), 1e-4, 1e-4)


def test_compare_is_per_element():
    # Element 0: big value, abs error 5e-4 but within rtol (5e-5 relative).
    # Element 1: small but significant value, relative error 4e-2 but within
    # atol (abs error 8e-5). Each one passes np.allclose; the two maxima
    # together would not.
    expected = np.array([10.0, 2e-3])
    actual = np.array([10.0005, 2.08e-3])
    e = split.compare(expected, actual, atol=1e-4, rtol=1e-4)
    assert e.max_abs > 1e-4 and e.max_rel > 1e-4
    assert e.equivalent


def test_chain_matches_the_model(tiny):
    model = split.load_frozen_model(tiny["saved"])
    boundaries = split.choose_boundaries(model)
    # One cut after each ReLU: the last one too, the reshape comes after it.
    assert [b.op_name.rsplit("/", 1)[-1] for b in boundaries] == [
        "stem_relu",
        "mid_relu",
        "last_relu",
    ]
    segments = split.convert_segments(model, boundaries)
    assert len(segments) == 4
    assert segments[0].input_shape[1:] == [8, 8, 3] and segments[-1].output_shape[1:] == [32, 6]
    x = split.random_sample((1, 8, 8, 3))
    assert split.verify(segments, split.frozen_reference(model), [x], 1e-5, 1e-5).equivalent


def test_requested_cuts(tiny):
    model = split.load_frozen_model(tiny["saved"])
    auto = split.choose_boundaries(model)
    again = split.choose_boundaries(model, requested=[auto[1].tensor_name])
    assert [b.tensor_name for b in again] == [auto[1].tensor_name]
    with pytest.raises(split.SplitError, match="not safe"):
        split.choose_boundaries(model, requested=["no/such:0"])


def test_image_sample(tmp_path):
    x = split.image_sample(sample_png(tmp_path / "s.png"), 8, 8)
    assert x.shape == (1, 8, 8, 3) and x.dtype == np.float32
    assert 0.0 <= x.min() and x.max() <= 1.0


# --- edge_impulse -----------------------------------------------------------------------


def test_define():
    header = METADATA.format(last_layer="X", quantized=0, dtype="D")
    assert edge_impulse.define(header, "EI_CLASSIFIER_INPUT_WIDTH") == "8"
    # A shorter name must not match a longer one with the same prefix.
    assert edge_impulse.define(header, "EI_CLASSIFIER_LOAD_FFT_3") == "1"
    assert edge_impulse.define("#define A 0.5 // comment\n", "A") == "0.5"
    with pytest.raises(edge_impulse.ExportError, match="EI_NOT_THERE"):
        edge_impulse.define(header, "EI_NOT_THERE")
    # Redefined inside an #if: the value depends on the build flags.
    redefined = "#define ENGINE 2\n#if FULL\n#undef ENGINE\n#define ENGINE 4\n#endif\n"
    with pytest.raises(edge_impulse.ExportError, match="defined 2 times"):
        edge_impulse.define(redefined, "ENGINE")
    assert edge_impulse.define("#define SAME 1\n#define SAME 1\n", "SAME") == "1"


def test_labels_and_nms():
    assert edge_impulse.labels(VARIABLES) == ["bicycle", "stop sign"]
    assert edge_impulse.nms_iou(VARIABLES) == pytest.approx(0.2)
    assert edge_impulse.nms_iou("no nms here") is None
    with pytest.raises(edge_impulse.ExportError):
        edge_impulse.labels("const char* other[] = { };")


@pytest.mark.parametrize("as_zip", [False, True], ids=["directory", "zip"])
def test_read_cpp_export(tiny, tmp_path, as_zip):
    path = zip_dir(tiny["export"], tmp_path / "cpp.zip") if as_zip else tiny["export"]
    export = edge_impulse.read_cpp_export(path)
    assert (export.project_id, export.deploy_version, export.studio_version) == (
        1011188,
        8,
        "1.94.5",
    )
    assert export.decoder == "yolo_pro" and not export.quantized
    assert (export.input_dtype, export.output_dtype) == ("float32", "float32")
    assert export.postprocess() == {
        "decoder": "yolo_pro",
        "labels": ["bicycle", "stop sign"],
        "score_threshold": 0.5,
        "max_detections": 200,  # Edge Impulse's top-K, not OBJECT_DETECTION_COUNT (10)
        "iou_threshold": pytest.approx(0.2),
    }
    assert export.full_model[4:8] == b"TFL3"  # a TFLite flatbuffer


def test_unknown_datatype(tiny, tmp_path):
    path = fake_export(tmp_path / "cpp", tiny["saved"], dtype="EI_CLASSIFIER_DATATYPE_FLOAT64")
    with pytest.raises(edge_impulse.ExportError, match="unknown data type"):
        edge_impulse.read_cpp_export(path)


def test_unsupported_family(tiny, tmp_path):
    path = fake_export(tmp_path / "cpp", tiny["saved"], last_layer="EI_CLASSIFIER_LAST_LAYER_SSD")
    with pytest.raises(edge_impulse.ExportError, match="unsupported last layer"):
        edge_impulse.read_cpp_export(path)


# --- manifest ---------------------------------------------------------------------------


def _manifest_parts(tiny):
    model = split.load_frozen_model(tiny["saved"])
    boundaries = split.choose_boundaries(model)
    segments = split.convert_segments(model, boundaries)
    errors = split.Errors(1e-6, 2e-6, True)
    return {
        "segments": segments,
        "boundaries": boundaries,
        "postprocess": {"decoder": "yolo_pro", "labels": ["a", "b"]},
        "source": manifest.Source("model.zip", "ab" * 32, 1011188, 8, "1.94.5"),
        "verification": manifest.Verification(2, 1e-4, 1e-4, errors, errors),
    }


def test_build_manifest(tiny):
    parts = _manifest_parts(tiny)
    m = manifest.build_manifest(**parts)
    assert list(m) == [
        "format_version",
        "quantization",
        "tool",
        "source",
        "input",
        "output",
        "cuts",
        "segments",
        "verification",
        "postprocess",
    ]
    assert m["format_version"] == 2 and m["quantization"] is None
    assert m["tool"] == {"name": "sciot-model", "tensorflow": tf.__version__}
    assert m["source"]["savedmodel"] == {"file": "model.zip", "sha256": "ab" * 32}
    assert m["input"] == {"shape": [1, 8, 8, 3], "dtype": "float32"}
    assert m["output"] == {"shape": [1, 32, 6], "dtype": "float32"}
    assert [c["operation_type"] for c in m["cuts"]] == ["Relu"] * 3
    assert [s["file"] for s in m["segments"]] == [f"submodel_{i}.tflite" for i in range(4)]
    assert m["verification"]["edge_impulse"] == {"max_abs_error": 1e-6, "max_rel_error": 2e-6}
    json.dumps(m)  # serializable as is


def test_write_model_dir(tiny, tmp_path):
    parts = _manifest_parts(tiny)
    out = tmp_path / "models" / "tiny"
    written = manifest.write_model_dir(out, parts["segments"], manifest.build_manifest(**parts))
    assert written["model_hash"] == model_hash(out)  # the same rule as the Go registry
    assert json.loads((out / "manifest.json").read_text()) == written
    assert sorted(p.name for p in tmp_path.joinpath("models").iterdir()) == [
        "tiny"
    ]  # no staging left

    with pytest.raises(FileExistsError, match="--overwrite"):
        manifest.write_model_dir(out, parts["segments"], manifest.build_manifest(**parts))
    manifest.write_model_dir(
        out, parts["segments"], manifest.build_manifest(**parts), overwrite=True
    )

    (out / "notes.txt").write_text("not a model file")
    with pytest.raises(FileExistsError, match="not part of a model"):
        manifest.write_model_dir(
            out, parts["segments"], manifest.build_manifest(**parts), overwrite=True
        )
    assert (out / "notes.txt").exists()


# --- cli --------------------------------------------------------------------------------


def test_build_end_to_end(tiny, tmp_path, capsys):
    out = tmp_path / "tiny_model"
    png = sample_png(tmp_path / "sample.png")
    assert cli.main(build_args(tiny, out, "--sample", str(png))) == 0

    m = json.loads((out / "manifest.json").read_text())
    assert m["model_hash"] == model_hash(out)
    assert m["postprocess"]["decoder"] == "yolo_pro"
    assert m["verification"]["samples"] == 1
    assert "/" not in m["source"]["savedmodel"]["file"]  # no local path
    printed = capsys.readouterr().out
    assert m["model_hash"] in printed and "THIS build" in printed


@pytest.mark.parametrize("dtype", ["EI_CLASSIFIER_DATATYPE_INT8", "EI_CLASSIFIER_DATATYPE_UINT8"])
def test_build_rejects_quantized_export(tiny, tmp_path, capsys, dtype):
    # Any quantized precision, not only int8.
    export = fake_export(tmp_path / "cpp", tiny["saved"], quantized=1, dtype=dtype)
    args = [
        "build",
        str(tiny["saved"]),
        "--ei-cpp-export",
        str(export),
        "--output",
        str(tmp_path / "o"),
    ]
    assert cli.main(args) == 2
    err = capsys.readouterr().err
    assert "float32 only" in err and dtype.rsplit("_", 1)[-1].lower() in err


def test_build_rejects_exports_of_different_models(tiny, tmp_path, capsys):
    # Same shapes, different weights: only the comparison with Edge
    # Impulse's .tflite can tell.
    other = save(tmp_path / "other", scale=3.0)
    export = fake_export(tmp_path / "cpp", other)
    args = [
        "build",
        str(tiny["saved"]),
        "--ei-cpp-export",
        str(export),
        "--output",
        str(tmp_path / "o"),
    ]
    assert cli.main(args) == 2
    assert "same model version" in capsys.readouterr().err
    assert not (tmp_path / "o").exists()


def test_check_consistency(tiny):
    model = split.load_frozen_model(tiny["saved"])
    export = edge_impulse.read_cpp_export(tiny["export"])
    cli.check_consistency(export, model)

    from dataclasses import replace

    with pytest.raises(cli.BuildError, match="16"):
        cli.check_consistency(replace(export, input_width=16), model)
    with pytest.raises(cli.BuildError, match="labels|columns|6"):
        cli.check_consistency(replace(export, labels=["only one"]), model)
    with pytest.raises(cli.BuildError):
        cli.check_consistency(replace(export, last_layer="EI_CLASSIFIER_LAST_LAYER_FOMO"), model)
