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
        "input": {"shape": shape, "dtype": "float32", "resize_mode": "fit_shortest"},
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


@pytest.mark.parametrize("resize_mode", [None, "crop"])
def test_rejects_a_manifest_without_a_known_resize_mode(tmp_path, resize_mode):
    import json

    inp = {"shape": [1, 4, 6, 3], "dtype": "float32"}
    if resize_mode is not None:
        inp["resize_mode"] = resize_mode
    (tmp_path / "manifest.json").write_text(
        json.dumps({"format_version": 2, "input": inp, "segments": []})
    )
    with pytest.raises(ValueError, match="rebuild"):
        segments.Model(tmp_path)


# --- to_model_input -------------------------------------------------------------------

RED, GREEN, BLUE = (255, 0, 0), (0, 255, 0), (0, 0, 255)
SQUARE = (1, 4, 4, 3)


def bands(rows: int, cols: int, colors, axis: int) -> np.ndarray:
    """A uint8 frame [rows, cols, 3] split into equal bands of colors along axis."""
    frame = np.zeros((rows, cols, 3), np.uint8)
    n = frame.shape[axis] // len(colors)
    for i, color in enumerate(colors):
        index = [slice(None), slice(None)]
        index[axis] = slice(i * n, (i + 1) * n)
        frame[tuple(index)] = color
    return frame


def to_input(frame, resize_mode, shape=SQUARE):
    return segments.to_model_input(frame, shape, np.dtype("float32"), resize_mode)


def test_output_shape_dtype_and_range():
    x = to_input(np.full((10, 7, 3), 255, np.uint8), "squash", (1, 4, 6, 3))
    assert x.shape == (1, 4, 6, 3) and x.dtype == np.float32
    assert np.allclose(x, 1.0)


@pytest.mark.parametrize("resize_mode", segments.RESIZE_MODES)
def test_a_frame_of_the_input_size_is_not_resized(resize_mode):
    frame = np.random.default_rng(0).integers(0, 256, (4, 4, 3), dtype=np.uint8)
    assert np.array_equal(to_input(frame, resize_mode)[0], frame.astype(np.float32) / 255.0)


@pytest.mark.parametrize(
    "frame",
    [
        bands(4, 8, [BLUE, RED, RED, GREEN], axis=1),  # landscape: crop left and right
        bands(8, 4, [BLUE, RED, RED, GREEN], axis=0),  # portrait (stop.png): top and bottom
    ],
    ids=["landscape", "portrait"],
)
def test_fit_shortest_crops_the_center(frame):
    # The centered 4x4 box is all red; no scaling, so the values are exact.
    x = to_input(frame, "fit_shortest")
    assert np.array_equal(x[0], np.broadcast_to(np.float32([1, 0, 0]), (4, 4, 3)))


def test_fit_longest_pads_with_black():
    # 8x4 red → scaled to 4 wide x 2 high, centered: rows 1-2 red, rows 0 and 3 black.
    x = to_input(np.full((4, 8, 3), RED, np.uint8), "fit_longest")[0]
    assert np.allclose(x[1:3], [1, 0, 0])
    assert np.array_equal(x[[0, 3]], np.zeros((2, 4, 3), np.float32))


def test_fit_longest_portrait_pads_left_and_right():
    x = to_input(np.full((8, 4, 3), RED, np.uint8), "fit_longest")[0]
    assert np.allclose(x[:, 1:3], [1, 0, 0])
    assert np.array_equal(x[:, [0, 3]], np.zeros((4, 2, 3), np.float32))


def test_squash_keeps_the_whole_frame():
    # Stretched, not cropped: the blue and green bands are still there.
    x = to_input(bands(4, 8, [BLUE, RED, RED, GREEN], axis=1), "squash")[0]
    # Bilinear averages the edge with its neighbour when shrinking: blue/green dominate, not pure.
    assert (x[:, 0, 2] > 0.8).all() and (x[:, -1, 1] > 0.8).all()


def test_resampling_is_bilinear():
    from PIL import Image

    frame = np.random.default_rng(1).integers(0, 256, (10, 7, 3), dtype=np.uint8)
    expected = Image.fromarray(frame).resize((4, 4), Image.Resampling.BILINEAR)
    assert np.array_equal(to_input(frame, "squash")[0], np.asarray(expected, np.float32) / 255.0)


def test_none_requires_the_input_size():
    with pytest.raises(ValueError):
        to_input(np.zeros((8, 8, 3), np.uint8), "none")


@pytest.mark.parametrize(
    "frame",
    [np.zeros((4, 4), np.uint8), np.zeros((4, 4, 4), np.uint8), np.zeros((4, 4, 3), np.float32)],
    ids=["gray", "rgba", "float"],
)
def test_rejects_a_frame_that_is_not_rgb_uint8(frame):
    with pytest.raises(ValueError, match="RGB uint8"):
        to_input(frame, "squash")


def test_rejects_an_unknown_resize_mode():
    with pytest.raises(ValueError, match="resize_mode"):
        to_input(np.zeros((4, 4, 3), np.uint8), "crop")
