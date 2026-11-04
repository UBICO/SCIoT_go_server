"""Decoders: the model output → detections, for LOCAL frames.

Same algorithm as the worker (worker/sciot_worker/decoders, specified in
docs/MODELS.md, "Decoders"). The implementations are kept identical by the
golden files in testdata/decoders, not by shared code: the client must not
depend on the worker.

"""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np


@dataclass(frozen=True)
class Params:
    """The "postprocess" of the model, as the server sends it."""

    decoder: str
    labels: tuple[str, ...]
    score_threshold: float
    max_detections: int
    iou_threshold: float | None = None  # None for decoders without NMS


@dataclass(frozen=True)
class Detection:
    """A found object. Box normalized to [0, 1], (x, y) = top-left corner."""

    label: str
    score: float
    x: float
    y: float
    w: float
    h: float


def decode(params: Params, output: np.ndarray) -> list[Detection]:
    """Decode one model output.

    Raises ValueError for an unknown decoder or an output that does not match
    the parameters (wrong number of columns).
    """
    from . import (
        yolo_pro,
    )  # here, not at the top: yolo_pro imports Params and Detection

    decoders = {"yolo_pro": yolo_pro.decode}
    if params.decoder not in decoders:
        raise ValueError(
            f"unknown decoder {params.decoder!r}; known: {', '.join(sorted(decoders))}"
        )
    return decoders[params.decoder](params, output)
