"""Decoders against the golden files of testdata/decoders (the specification)."""

from __future__ import annotations

import json
from pathlib import Path

import numpy as np
import pytest

from sciot_worker import decoders
from sciot_worker.decoders import yolo_pro

GOLDEN = Path(__file__).resolve().parents[2] / "testdata" / "decoders" / "yolo_pro"
SYNTHETIC = sorted(p for p in GOLDEN.glob("*.json") if not p.with_suffix(".bin").exists())


def params(pp: dict) -> decoders.Params:
    return decoders.Params(
        decoder=pp["decoder"],
        labels=tuple(pp["labels"]),
        score_threshold=pp["score_threshold"],
        max_detections=pp["max_detections"],
        iou_threshold=pp.get("iou_threshold"),
    )


def assert_detections(got: list[decoders.Detection], want: list[dict]) -> None:
    assert len(got) == len(want), f"{len(got)} detections, want {len(want)}: {got}"
    for i, (g, w) in enumerate(zip(got, want)):
        assert g.label == w["label"], f"detection {i}: label {g.label!r}, want {w['label']!r}"
        assert g.score == pytest.approx(w["score"], abs=1e-6), f"detection {i}: score"
        box = (g.x, g.y, g.w, g.h)
        assert box == pytest.approx(tuple(w["box"][k] for k in "xywh"), abs=1e-5), (
            f"detection {i}: box"
        )


def test_there_are_golden_files():
    assert len(SYNTHETIC) >= 10, f"golden files not found in {GOLDEN}"


@pytest.mark.parametrize("path", SYNTHETIC, ids=lambda p: p.stem)
def test_synthetic_golden(path):
    case = json.loads(path.read_text())
    # float32, like the real model output: the values are exact in binary.
    output = np.asarray(case["rows"], dtype=np.float32)[np.newaxis]
    got = decoders.decode(params(case["postprocess"]), output)
    assert_detections(got, case["detections"])
    assert all(isinstance(d.score, float) and isinstance(d.x, float) for d in got)


def test_prepare_does_not_change_the_input():
    output = np.array([[-1.0, 0, 2.0, 1, 0.9, 0]], dtype=np.float32)
    before = output.copy()
    yolo_pro.prepare(output, 2)
    np.testing.assert_array_equal(output, before)


def test_non_finite_rows_are_never_candidates():
    # Not a golden file: JSON cannot hold NaN. Row 0 has a NaN box and the best
    # score, row 1 an infinite score; only row 2 is a detection.
    output = np.array(
        [[np.nan, 0, 1, 1, 0.875, 0], [0, 0, 1, 1, np.inf, 0], [0, 0, 0.5, 0.5, 0.75, 0]],
        dtype=np.float32,
    )
    got = decoders.decode(
        decoders.Params("yolo_pro", ("bicycle", "stop sign"), 0.5, 200, 0.5), output
    )
    assert [(d.label, d.score) for d in got] == [("bicycle", 0.75)]
    assert all(np.isfinite([d.score, d.x, d.y, d.w, d.h]).all() for d in got)


def test_prepare_rejects_wrong_columns():
    with pytest.raises(ValueError, match="7"):
        yolo_pro.prepare(np.zeros((1, 3, 7), dtype=np.float32), 2)
    with pytest.raises(ValueError):
        yolo_pro.prepare(np.zeros(13, dtype=np.float32), 2)  # not a multiple of 6


def test_prepare_accepts_a_flat_buffer():
    # The worker decodes the raw output bytes: a 1-D float32 array.
    rows = yolo_pro.prepare(np.zeros(12, dtype=np.float32), 2)
    assert rows.shape == (2, 6) and rows.dtype == np.float64


def test_iou():
    a = np.array([0, 0, 0.75, 1.0])
    b = np.array([0.25, 0, 1.0, 1.0])
    assert yolo_pro.iou(a, b) == pytest.approx(0.5)
    assert yolo_pro.iou(a, a) == pytest.approx(1.0)
    assert yolo_pro.iou(a, np.array([0.8, 0, 1, 1])) == 0.0
    assert yolo_pro.iou(a, np.array([0.5, 0, 0.5, 1])) == 0.0  # zero area


def test_unknown_decoder():
    p = decoders.Params("ssd", ("a",), 0.5, 10)
    with pytest.raises(ValueError, match="unknown decoder"):
        decoders.decode(p, np.zeros((1, 5), dtype=np.float32))
