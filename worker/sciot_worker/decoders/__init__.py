"""Decoders: the model output → detections (docs/MODELS.md, "Decoders").

The same algorithm runs on the server (here, for SPLIT frames) and on the
devices (LOCAL frames): the Python client and the ESP32 have their own
implementations, kept identical by the golden files in testdata/decoders/.
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
    from . import yolo_pro  # here, not at the top: yolo_pro imports Params and Detection

    decoders = {"yolo_pro": yolo_pro.decode}
    if params.decoder not in decoders:
        raise ValueError(
            f"unknown decoder {params.decoder!r}; known: {', '.join(sorted(decoders))}"
        )
    return decoders[params.decoder](params, output)
