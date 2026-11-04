from pathlib import Path

import numpy as np
import pytest

from sciot_client import segments

TESTDATA = Path(__file__).resolve().parents[2] / "testdata"


def test_model_hash_matches_testdata():
    # The same expected hash as the Go registry and the worker.
    d = TESTDATA / "model_hash"
    assert segments.model_hash(d) == (d / "expected_hash").read_text().strip()


def test_model_hash_empty_dir(tmp_path):
    with pytest.raises(FileNotFoundError):
        segments.model_hash(tmp_path)


@pytest.fixture(scope="module")
def model(tmp_path_factory):
    """A 3-segment float32 model: x+1, x*2, x-3 on a [1, 4, 6, 3] input."""
    tf = pytest.importorskip("tensorflow")
    import json

    d = tmp_path_factory.mktemp("model")
    shape = [1, 4, 6, 3]
    ops = [lambda x: x + 1, lambda x: x * 2, lambda x: x - 3]
    entries = []
    for i, op in enumerate(ops):
        fn = tf.function(op, input_signature=[tf.TensorSpec(shape, tf.float32)])
        conv = tf.lite.TFLiteConverter.from_concrete_functions([fn.get_concrete_function()], fn)
        (d / f"submodel_{i}.tflite").write_bytes(conv.convert())
        io = {"shape": shape, "dtype": "float32"}
        entries.append({"index": i, "file": f"submodel_{i}.tflite", "input": io, "output": io})
    manifest = {
        "format_version": 2,
        "input": {"shape": shape, "dtype": "float32"},
        "segments": list(reversed(entries)),  # the index decides the order, not the list
    }
    (d / "manifest.json").write_text(json.dumps(manifest))
    return segments.Model(d)


def test_run_prefix_completion_and_nothing(model):
    x = np.zeros((1, 4, 6, 3), np.float32)
    assert model.last_layer == 2
    out, seconds = model.run(x, 0, 2)
    assert np.allclose(out, (0 + 1) * 2 - 3) and len(seconds) == 3
    prefix, _ = model.run(x, 0, 0)
    rest, seconds = model.run(prefix, 1, 2)
    assert np.allclose(rest, out) and len(seconds) == 2
    same, seconds = model.run(x, 0, -1)  # split point -1: nothing on the device
    assert same is x and seconds == []
    with pytest.raises(ValueError):
        model.run(x, 0, 3)


def test_load_input_resizes_width_and_height(model, tmp_path):
    from PIL import Image

    path = tmp_path / "img.png"
    Image.new("RGB", (40, 10), (255, 0, 0)).save(path)
    x = model.load_input(path)
    assert x.shape == (1, 4, 6, 3) and x.dtype == np.float32
    assert np.allclose(x[..., 0], 1.0) and np.allclose(x[..., 1:], 0.0)


def test_rejects_another_manifest_version(tmp_path):
    (tmp_path / "manifest.json").write_text('{"format_version": 1}')
    with pytest.raises(ValueError, match="format_version"):
        segments.Model(tmp_path)
