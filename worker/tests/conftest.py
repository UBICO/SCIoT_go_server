"""Tiny TFLite models built at test time: no model artifacts needed.

Segment 0 maps a (1, 6) input to (1, 4); segments 1..N-1 map (1, 4) to (1, 4)
as y = x + i (identity kernel, bias i). The output of a chain is therefore
known exactly: running segments k+1..last on x gives x + sum(k+1..last).
11 segments, so that NAME order (submodel_10 before submodel_2) differs from
EXECUTION order, as in the real FOMO models (59 segments).
"""

from __future__ import annotations

import numpy as np
import pytest
import tensorflow as tf

NUM_SEGMENTS = 11


def _convert(model: tf.keras.Model) -> bytes:
    return tf.lite.TFLiteConverter.from_keras_model(model).convert()


def _segment(index: int) -> bytes:
    if index == 0:
        inp = tf.keras.Input(shape=(6,), batch_size=1)
        dense = tf.keras.layers.Dense(4)
        out = dense(inp)
        model = tf.keras.Model(inp, out)
        kernel = np.zeros((6, 4), dtype=np.float32)
        kernel[:4, :4] = np.eye(4)  # passes the first 4 values through
        dense.set_weights([kernel, np.zeros(4, dtype=np.float32)])
        return _convert(model)
    inp = tf.keras.Input(shape=(4,), batch_size=1)
    dense = tf.keras.layers.Dense(4)
    out = dense(inp)
    model = tf.keras.Model(inp, out)
    dense.set_weights([np.eye(4, dtype=np.float32), np.full(4, index, dtype=np.float32)])
    return _convert(model)


@pytest.fixture(scope="session")
def model_dir(tmp_path_factory):
    directory = tmp_path_factory.mktemp("tiny_model")
    for i in range(NUM_SEGMENTS):
        (directory / f"submodel_{i}.tflite").write_bytes(_segment(i))
    return directory


def expected_output(x: np.ndarray, split_point: int) -> np.ndarray:
    """Output of segments split_point+1 .. last for an input of segment split_point+1."""
    y = x[:, :4] if split_point == -1 else x
    return y + sum(range(max(split_point + 1, 1), NUM_SEGMENTS))
