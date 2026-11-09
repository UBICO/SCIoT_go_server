"""Camera frame sources: OpenCV (laptop) and Picamera2 (Raspberry Pi).

Each camera has a background thread that reads frames continuously and keeps
only the newest (``LatestFrame``): the inference loop never gets a frame
that waited in a driver buffer, and ``read()`` returns as soon as a frame
newer than the previous one exists. ``read()`` gives the frame (RGB uint8
[H, W, 3]), ``captured_at`` (wall clock, RFC 3339) and ``captured_t``
(time.perf_counter), both taken when the thread received it: the latency of a
frame starts there, not when the loop asked for it (FrameRecord.age_ms).

Transient read errors are retried in the thread; after MAX_READ_ERRORS in a
row, or no frame for FRAME_TIMEOUT_S, ``read()`` raises and the client stops.

The libraries are imported in the constructors, so this module loads where
they are missing: OpenCV is an optional extra (pyproject.toml), Picamera2
comes from Raspberry Pi OS (docs/RASPBERRY.md).

The static image is not a frame source: http_go_client prepares it once.
"""

from __future__ import annotations

import logging
import sys
import threading
import time
from collections.abc import Callable

import numpy as np

from sciot_client.split_pipeline import utc_now

logger = logging.getLogger(__name__)

MAX_READ_ERRORS = 10  # consecutive
FRAME_TIMEOUT_S = 2.0


class FrameError(RuntimeError):
    """No frame from the camera: too many read errors, or none for too long."""


class LatestFrame:
    """A thread that calls ``grab()`` in a loop and keeps the newest frame.

    ``grab()`` returns an RGB uint8 frame or raises on a read error. It may
    block until the camera has a frame (both libraries do).
    """

    def __init__(
        self,
        grab: Callable[[], np.ndarray],
        *,
        name: str = "camera",
        max_errors: int = MAX_READ_ERRORS,
        timeout_s: float = FRAME_TIMEOUT_S,
        clock: Callable[[], str] = utc_now,
        monotonic: Callable[[], float] = time.perf_counter,  # run_loop's clock
    ):
        self._grab = grab
        self._max_errors = max_errors
        self._timeout_s = timeout_s
        self._clock = clock
        self._monotonic = monotonic
        # Everything below is shared with the thread: only under _cond.
        self._cond = threading.Condition()
        self._frame: np.ndarray | None = None
        self._captured_at = ""
        self._captured_t = 0.0  # self._monotonic() at the same instant
        self._seq = 0  # frames received so far
        self._read_seq = 0  # seq of the last frame read() returned
        self._error: BaseException | None = None  # set once, when the thread gives up
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._run, name=name, daemon=True)
        self._thread.start()

    def _run(self) -> None:
        errors = 0  # consecutive
        while not self._stop.is_set():
            try:
                frame = self._grab()  # outside the lock: it blocks for up to a frame
            except Exception as e:  # noqa: BLE001 - any driver error is retried
                errors += 1
                logger.warning(
                    "%s: read error %d/%d: %s", self._thread.name, errors, self._max_errors, e
                )
                if errors >= self._max_errors:
                    with self._cond:
                        self._error = e
                        self._cond.notify_all()
                    return
                continue
            errors = 0
            if self._stop.is_set():  # close() came while grab() blocked
                return
            with self._cond:
                self._frame = frame
                self._captured_at = self._clock()
                self._captured_t = self._monotonic()
                self._seq += 1
                self._cond.notify_all()

    def read(self) -> tuple[np.ndarray, str, float]:
        """The first frame newer than the previous one: (frame, captured_at, captured_t).

        Raises FrameError when the thread gave up (cause: the last read
        error) or no new frame arrived within timeout_s.
        """
        with self._cond:
            arrived = self._cond.wait_for(
                lambda: self._seq > self._read_seq or self._error is not None,
                timeout=self._timeout_s,
            )
            if self._error is not None:
                raise FrameError(f"{self._thread.name}: {self._error}") from self._error
            if not arrived:
                raise FrameError(f"{self._thread.name}: no frame in {self._timeout_s} s")
            self._read_seq = self._seq
            return self._frame, self._captured_at, self._captured_t

    def close(self) -> None:
        """Stop the thread; waits for the grab() in progress (about a frame)."""
        self._stop.set()
        self._thread.join(timeout=self._timeout_s)


class OpenCVCamera:
    """A camera through OpenCV (laptop webcam); ``index`` as in cv2.VideoCapture."""

    def __init__(self, index: int = 0):
        import cv2  # optional extra: uv sync --extra opencv

        # AVFoundation on macOS: the default backend there may not open webcams.
        if sys.platform == "darwin":
            cap = cv2.VideoCapture(index, cv2.CAP_AVFOUNDATION)
        else:
            cap = cv2.VideoCapture(index)
        if not cap.isOpened():
            cap.release()
            raise FrameError(
                f"cannot open camera {index}. On macOS allow the terminal in System "
                "Settings > Privacy & Security > Camera"
            )
        self._cap = cap

        def grab() -> np.ndarray:
            ok, frame = cap.read()
            if not ok:
                raise FrameError(f"camera {index}: read failed")
            return cv2.cvtColor(frame, cv2.COLOR_BGR2RGB)  # OpenCV frames are BGR

        self._latest = LatestFrame(grab, name=f"opencv-{index}")

    def read(self) -> tuple[np.ndarray, str, float]:
        return self._latest.read()

    def close(self) -> None:
        self._latest.close()  # first: grab() must not run on a released camera
        self._cap.release()


def isp_config(
    resize_mode: str,
    crop_max: tuple[int, int, int, int],
    input_size: tuple[int, int],
) -> tuple[tuple[int, int, int, int], tuple[int, int]]:
    """Where the Raspberry Pi ISP crops the sensor, and the size it outputs.

    ``crop_max`` is the sensor area (x, y, width, height), Picamera2's
    ScalerCropMaximum; ``input_size`` is the model's (width, height). Returns
    (ScalerCrop, output size), so that segments.to_model_input does not
    resize:

    - fit_shortest: the largest centered box with the input aspect ratio,
      output = input size;
    - squash and none: the whole sensor, output = input size (the ISP
      stretches; for none the frame has the input size, as it requires);
    - fit_longest: the whole sensor, output = the input size reduced to the
      sensor aspect ratio, sizes truncated to int as in the SDK
      (to_model_input then pads it, without resizing).
    """
    x, y, width, height = crop_max
    in_w, in_h = input_size
    if resize_mode in ("squash", "none"):
        return crop_max, input_size
    # Sensor wider than the input (compared without division, in integers).
    wider = width * in_h > height * in_w
    if resize_mode == "fit_shortest":
        box_w, box_h = (height * in_w // in_h, height) if wider else (width, width * in_h // in_w)
        return (x + (width - box_w) // 2, y + (height - box_h) // 2, box_w, box_h), input_size
    if resize_mode == "fit_longest":
        # The side that fits is the one where the sensor is relatively longer; the
        # other follows the sensor ratio (same rule as segments.to_model_input).
        size = (in_w, in_w * height // width) if wider else (in_h * width // height, in_h)
        return crop_max, size
    raise ValueError(f"unknown resize_mode {resize_mode!r}")


class PiCamera:
    """The Raspberry Pi camera through Picamera2; the ISP crops and scales.

    The frame comes out of the ISP already cropped and scaled for
    ``resize_mode`` (``isp_config``): no resize on the CPU, which the model
    needs.
    """

    def __init__(self, input_size: tuple[int, int], resize_mode: str, index: int = 0):
        # From apt (python3-picamera2), not PyPI: the venv needs
        # --system-site-packages (docs/RASPBERRY.md).
        from picamera2 import Picamera2

        cam = Picamera2(index)  # index: the connector on a Pi 5 with two cameras
        # ScalerCropMaximum is the area of the sensor mode libcamera picks in
        # configure(): (0, 0, 0, 0) before it (seen on a Pi, 2026-11-09).
        # RGB888 is ordered [B, G, R] in memory (Picamera2 manual): reversed in grab().
        config = cam.create_video_configuration(main={"size": input_size, "format": "RGB888"})
        cam.configure(config)
        crop_max = tuple(cam.camera_properties["ScalerCropMaximum"])
        crop, size = isp_config(resize_mode, crop_max, input_size)
        if size != input_size:  # fit_longest: the output size depends on the area
            config["main"]["size"] = size
            cam.configure(config)
            if tuple(cam.camera_properties["ScalerCropMaximum"]) != crop_max:
                area = tuple(cam.camera_properties["ScalerCropMaximum"])
                cam.close()  # nothing else will: __init__ does not return
                raise FrameError(
                    f"sensor area changed from {crop_max} to {area} after "
                    f"reconfiguring to {size}: the crop would be wrong"
                )
        cam.set_controls({"ScalerCrop": crop})
        cam.start()
        self._cam = cam
        logger.info("picamera2: sensor area %s, crop %s, output %s", crop_max, crop, size)

        def grab() -> np.ndarray:
            return cam.capture_array("main")[:, :, ::-1]

        self._latest = LatestFrame(grab, name=f"picamera2-{index}")

    def read(self) -> tuple[np.ndarray, str, float]:
        return self._latest.read()

    def close(self) -> None:
        self._latest.close()  # first: grab() must not run on a stopped camera
        self._cam.stop()
        self._cam.close()
