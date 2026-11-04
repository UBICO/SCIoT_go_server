"""YOLO-Pro (Edge Impulse) decoder.

The output has one row per candidate box:

    xmin, ymin, xmax, ymax, score_0, ..., score_{C-1}

with coordinates normalized to [0, 1] (values outside happen and are
clipped) and scores already probabilities. The algorithm is Edge Impulse's
(edge-impulse-sdk/classifier: fill_result_struct_yolo_pro_common, ei_nms.h),
with three deliberate differences, all in docs/MODELS.md:

- boxes are normalized floats, not integer pixels truncated by the SDK;
- ties in score are broken by row index (lowest first), then by class:
  the SDK leaves them to std::priority_queue / std::sort, whose order is
  not defined, and Python, Go and C must agree;
- no zero-score padding entries.

Edge Impulse computes the IoU in pixels; here it is computed on the
normalized coordinates. The value is the same: scaling x by the width and y
by the height multiplies both areas and the intersection by width * height.

Edge Impulse computes in float32, this code in float64. The golden files use
values exact in binary, so both give the same results on them.
"""

from __future__ import annotations

import numpy as np

from . import Detection, Params

BOX_COLUMNS = 4


def prepare(output: np.ndarray, num_labels: int) -> np.ndarray:
    """Rows as float64 (N, 4 + num_labels), with clipped and ordered corners."""
    columns = BOX_COLUMNS + num_labels
    # A flat buffer has no columns of its own; any other shape must end in them.
    if output.ndim > 1 and output.shape[-1] != columns:
        raise ValueError(
            f"output has {output.shape[-1]} columns, "
            f"yolo_pro with {num_labels} labels needs {columns}"
        )
    if output.size % columns:
        raise ValueError(
            f"output has {output.size} values, not a multiple of the {columns} columns "
            f"yolo_pro with {num_labels} labels needs"
        )
    # reshape of a float64 array may return a view: astype(copy=True) always copies.
    rows = output.reshape(-1, columns).astype(np.float64, copy=True)
    # A row with a NaN or an infinity is never a candidate: a NaN box would
    # reach the JSON of the result, which cannot encode it. Zeroed rather than
    # removed, so that row indexes (the tie-break) stay the original ones.
    rows[~np.isfinite(rows).all(axis=1)] = 0.0
    rows[:, :BOX_COLUMNS] = np.clip(rows[:, :BOX_COLUMNS], 0.0, 1.0)
    rows[:, 2] = np.maximum(rows[:, 2], rows[:, 0])
    rows[:, 3] = np.maximum(rows[:, 3], rows[:, 1])
    return rows


def iou(a: np.ndarray, b: np.ndarray) -> float:
    """Intersection over union of two boxes (xmin, ymin, xmax, ymax)."""
    area_a = (a[2] - a[0]) * (a[3] - a[1])
    area_b = (b[2] - b[0]) * (b[3] - b[1])
    if area_a <= 0 or area_b <= 0:
        return 0.0
    intersection = max(0.0, min(a[2], b[2]) - max(a[0], b[0])) * max(
        0.0, min(a[3], b[3]) - max(a[1], b[1])
    )
    return float(intersection / (area_a + area_b - intersection))


def nms_class(rows: np.ndarray, column: int, params: Params) -> list[int]:
    """Row indexes kept for one class (score column `column`), best first."""
    scores = rows[:, column]
    candidates = [i for i in range(len(rows)) if params.score_threshold <= scores[i] <= 1.0]
    candidates.sort(key=lambda i: (-scores[i], i))
    if params.iou_threshold is None:
        return candidates

    kept: list[int] = []
    for i in candidates:
        box = rows[i, :BOX_COLUMNS]
        if all(iou(box, rows[k, :BOX_COLUMNS]) < params.iou_threshold for k in kept):
            kept.append(i)
    return kept


def decode(params: Params, output: np.ndarray) -> list[Detection]:
    rows = prepare(output, len(params.labels))
    found: list[tuple[float, int, int]] = []  # (score, row index, class index)
    for c in range(len(params.labels)):
        column = BOX_COLUMNS + c
        found.extend((rows[i, column], i, c) for i in nms_class(rows, column, params))
    found.sort(key=lambda f: (-f[0], f[1], f[2]))

    detections = []
    for score, i, c in found[: params.max_detections]:
        xmin, ymin, xmax, ymax = (float(v) for v in rows[i, :BOX_COLUMNS])
        detections.append(
            Detection(
                label=params.labels[c],
                score=float(score),
                x=xmin,
                y=ymin,
                w=xmax - xmin,
                h=ymax - ymin,
            )
        )
    return detections
