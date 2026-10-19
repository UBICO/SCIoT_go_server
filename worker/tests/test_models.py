import hashlib

import numpy as np
import pytest
from conftest import NUM_SEGMENTS, expected_output

from sciot_worker.models import LoadedModel, model_hash, segment_paths


def test_segment_paths_in_execution_order(model_dir):
    names = [p.name for p in segment_paths(model_dir)]
    assert names == [f"submodel_{i}.tflite" for i in range(NUM_SEGMENTS)]


def test_segment_paths_rejects_unindexed_files(tmp_path):
    (tmp_path / "model.tflite").write_bytes(b"x")
    with pytest.raises(ValueError):
        segment_paths(tmp_path)


def test_model_hash_uses_name_order(model_dir):
    by_name = sorted(model_dir.glob("*.tflite"))
    # Precondition of the test: with 11 segments the two orders differ.
    assert [p.name for p in by_name] != [p.name for p in segment_paths(model_dir)]

    expected = hashlib.md5(b"".join(p.read_bytes() for p in by_name)).hexdigest()
    assert model_hash(model_dir) == expected

    numeric = hashlib.md5(b"".join(p.read_bytes() for p in segment_paths(model_dir))).hexdigest()
    assert model_hash(model_dir) != numeric


def test_model_hash_empty_dir(tmp_path):
    with pytest.raises(FileNotFoundError):
        model_hash(tmp_path)


def test_specs(model_dir):
    specs = LoadedModel("tiny", model_dir, 1).specs()
    assert [s.index for s in specs] == list(range(NUM_SEGMENTS))
    assert specs[0].input_shape == (1, 6) and specs[0].output_shape == (1, 4)
    assert specs[1].input_shape == (1, 4)
    assert specs[0].input_dtype == np.dtype(np.float32)
    assert specs[0].input_bytes == 24 and specs[5].output_bytes == 16


@pytest.mark.parametrize("split_point", [-1, 0, 4, NUM_SEGMENTS - 2])
def test_run_from(model_dir, split_point):
    model = LoadedModel("tiny", model_dir, 1)
    width = 6 if split_point == -1 else 4
    x = np.arange(width, dtype=np.float32).reshape(1, width)

    output, layer_ms = model.run_from(split_point, x.tobytes())

    got = np.frombuffer(output, dtype=np.float32).reshape(1, 4)
    np.testing.assert_allclose(got, expected_output(x, split_point))
    assert len(layer_ms) == NUM_SEGMENTS - (split_point + 1)


@pytest.mark.parametrize("split_point", [-2, NUM_SEGMENTS - 1, 99])
def test_run_from_invalid_split(model_dir, split_point):
    with pytest.raises(ValueError, match="split point"):
        LoadedModel("tiny", model_dir, 1).run_from(split_point, b"\0" * 16)


def test_run_from_wrong_size(model_dir):
    with pytest.raises(ValueError, match="bytes"):
        LoadedModel("tiny", model_dir, 1).run_from(0, b"\0" * 15)
