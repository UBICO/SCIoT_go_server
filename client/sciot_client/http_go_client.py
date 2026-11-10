"""Inference client for the Go server (v1 control plane).

The inference loop never talks to the server:

- ``HeartbeatLoop`` registers the device and, in a background thread, sends
  the telemetry and receives the execution policy;
- this loop reads ``hb.policy`` before each inference, runs the model and
  records the inference time in the shared ``InferenceWindow``.

So there is no per-inference ``GET offloading_layer`` and, in LOCAL, no
per-inference upload.

In SPLIT the loop computes segments 0..k and ``split_pipeline.SplitPipeline``
sends the activation on the data plane stream without waiting: the next frame
is acquired while this one is on the server. When the server cannot take a
frame (window full, refused, stream down) the device finishes it itself.

Server, device id, model, frame source and image come from ``go_client.yaml``
(``go_client_config``), overridden by the command line. The model runs with
``segments.Model``, from the model directory's ``manifest.json``. The frames
come from a camera (``frames``) or, with ``--camera static``, from one image
prepared once.

Usage::

    uv run --project client sciot-client --model-dir ~/sciot-models/yolo_small_320
"""

from __future__ import annotations

import argparse
import logging
import threading
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any

from sciot_client import frames, go_client_config, segments
from sciot_client.control_plane_client import (
    MODE_LOCAL,
    MODE_SPLIT,
    ControlPlaneClient,
    InferenceWindow,
    Policy,
)
from sciot_client.heartbeat_loop import HeartbeatLoop
from sciot_client.split_pipeline import (
    FrameCSV,
    FrameTable,
    SegmentTimes,
    SplitPipeline,
    utc_now,
)

logger = logging.getLogger(__name__)


def stop_layer_for(policy: Policy, last_layer: int) -> int:
    """Index of the last layer to run on the device for this policy.

    LOCAL runs the whole model (``last_layer``). SPLIT runs up to and
    including ``policy.split_point``, in ``-1..last_layer - 1``: -1 runs
    nothing (the model input goes to the server), ``last_layer - 1`` leaves
    only the last segment to the server. ``last_layer`` would leave nothing
    to the server: the server never sends it (it is LOCAL).
    """
    if policy.mode == MODE_LOCAL:
        return last_layer
    if policy.mode == MODE_SPLIT:
        split_point = policy.split_point
        if split_point is None or not -1 <= split_point <= last_layer - 1:
            raise ValueError(
                f"split_point {split_point!r} outside the model (-1..{last_layer - 1})"
            )
        return split_point
    raise ValueError(f"unknown policy mode {policy.mode!r}")


def run_loop(
    get_policy: Callable[[], Policy],
    window: InferenceWindow,
    acquire: Callable[[], tuple[Any, str, float | None]],
    infer: Callable[[Any, int], float],
    *,
    last_layer: int,
    stop: threading.Event,
    max_cycles: int | None = None,
    clock: Callable[[], float] = time.perf_counter,
    on_cycle: Callable[[int, float, float], None] | None = None,
    before_cycle: Callable[[], None] | None = None,
    before_acquire: Callable[[Policy, int], None] | None = None,
    on_output: Callable[[Policy, int, Any, str, float, float, float], None] | None = None,
) -> int:
    """Run inferences until ``stop`` is set or ``max_cycles`` is reached.

    Each cycle:

    1. read the policy (once per cycle) and derive the stop layer;
    2. ``sample, captured_at, captured_t = acquire()``, timed with ``clock`` →
       acquisition time. ``captured_at`` (RFC 3339, ``split_pipeline.utc_now``)
       is when the frame was captured, which with a camera is before the call;
       ``captured_t`` is the same instant on ``clock`` (None: the frame did not
       exist before the call, the static image) → the frame's age when
       acquire() started (``age_ms``, negative if it arrived during the call);
    3. ``output, comp_ms = infer(sample, layer)`` → the output of segment
       ``layer`` and the compute time. ``infer`` measures the time itself, so
       that it sums only the interpreter invocations;
    4. ``window.record(comp_ms)`` and, if given, ``on_cycle(layer, acq_ms,
       comp_ms)``;
    5. if given, ``on_output(policy, layer, output, captured_at, acq_ms,
       comp_ms, age_ms)``: the SplitPipeline sends or finishes the frame.

    ``before_cycle`` runs first in every cycle: the SplitPipeline handles the
    answers received meanwhile (on this thread, not the receiver's).
    ``before_acquire(policy, layer)`` runs after the policy is read and before
    acquire(): with the window full the SplitPipeline may wait there for a
    place (docs/CLIENT.md, "Window full"), outside the acquisition time.

    Returns the number of completed inferences. The dependencies are
    parameters (not globals) so the loop can be tested without TensorFlow, a
    server or real time.
    """
    completed = 0
    while not stop.is_set() and (max_cycles is None or completed < max_cycles):
        if before_cycle is not None:
            before_cycle()
        # Read ONCE per cycle: the whole inference uses the same policy, even
        # if the heartbeat thread changes it meanwhile.
        policy = get_policy()
        layer = stop_layer_for(policy, last_layer)
        if before_acquire is not None:
            before_acquire(policy, layer)

        # acquire/infer exceptions are NOT caught: a broken camera or model
        # must stop the client (the camera retries transient errors itself).
        start = clock()
        sample, captured_at, captured_t = acquire()
        acq_ms = (clock() - start) * 1000
        # Signed on purpose: a frame that arrived while acquire() waited is
        # negative, so that age_ms + acq_ms is always capture → sample ready.
        age_ms = 0.0 if captured_t is None else (start - captured_t) * 1000

        # infer times itself (interpreter invocations only), so the loop does
        # not wrap it in clock().
        output, comp_ms = infer(sample, layer)

        window.record(comp_ms)
        if on_cycle is not None:
            on_cycle(layer, acq_ms, comp_ms)
        if on_output is not None:
            on_output(policy, layer, output, captured_at, acq_ms, comp_ms, age_ms)
        completed += 1
    return completed


def open_camera(cfg: go_client_config.GoClientConfig, model: segments.Model):
    """The frame source of cfg.camera, open; None for "static".

    ``PiCamera`` gets the model input size (width, height) and resize mode: its
    ISP crops and scales (frames.isp_config).
    """
    if cfg.camera == "static":
        return None
    if cfg.camera == "opencv":
        return frames.OpenCVCamera(cfg.camera_index)
    if cfg.camera == "picamera2":
        _, height, width, _ = model.input_shape  # [1, h, w, 3], not (w, h)
        return frames.PiCamera((width, height), model.resize_mode, cfg.camera_index)
    raise ValueError(f"unknown camera {cfg.camera!r}")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="SCIoT inference client for the Go server.")
    parser.add_argument(
        "--config",
        type=Path,
        help=f"config file (default: ${go_client_config.ENV_VAR}, else "
        "go_client.yaml next to this module)",
    )
    parser.add_argument("--server", help="Go server URL (default: from the config)")
    parser.add_argument("--device-id", help="default: device_id in the config")
    parser.add_argument("--cycles", type=int, help="stop after N inferences (default: run forever)")
    parser.add_argument(
        "--no-table", action="store_true", help="do not print the per-inference table"
    )
    parser.add_argument(
        "--model-dir",
        type=Path,
        help="model directory built by sciot-model build, the SAME one the server "
        "serves (default: model_dir in the config)",
    )
    parser.add_argument(
        "--image",
        type=Path,
        help="input image of --camera static (default: image in the config)",
    )
    parser.add_argument(
        "--camera",
        choices=go_client_config.CAMERAS,
        help="frame source: static (the image, prepared once), opencv (laptop "
        "webcam, needs the opencv extra) or picamera2 (Raspberry Pi) "
        "(default: camera in the config)",
    )
    parser.add_argument(
        "--camera-index",
        type=int,
        help="which camera, 0 = the first (default: camera_index in the config)",
    )
    parser.add_argument(
        "--no-probe",
        dest="probe",
        action="store_const",
        const=False,
        help="do not answer the server's bandwidth probes (default: probe in the config)",
    )
    parser.add_argument(
        "--frames-csv",
        type=Path,
        help="also write one CSV row per frame (for scripts/bench_split.py)",
    )
    parser.add_argument(
        "--deadline-ms",
        type=float,
        help="fixed deadline of SPLIT frames (default: the time this device "
        "would need to finish the frame itself, once measured)",
    )
    args = parser.parse_args(argv)
    try:
        cfg = go_client_config.load(
            args.config,
            {
                "server": args.server,
                "device_id": args.device_id,
                "model_dir": args.model_dir,
                "image": args.image,
                "probe": args.probe,
                "camera": args.camera,
                "camera_index": args.camera_index,
            },
        )
    except go_client_config.ConfigError as exc:
        parser.error(str(exc))

    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(message)s")
    logger.info(
        "config %s: server %s, device %s, model %s, camera %s",
        cfg.source or "(none)",
        cfg.server,
        cfg.device_id,
        cfg.model_dir,
        cfg.camera if cfg.camera == "static" else f"{cfg.camera} {cfg.camera_index}",
    )

    # Loads every segment before timing anything.
    model = segments.Model(cfg.model_dir)
    device_id = cfg.device_id
    last_layer = model.last_layer
    window = InferenceWindow()
    segment_times = SegmentTimes(window=window)

    def infer(x, stop_layer: int):
        # Sum of the interpreter invocations only. The per-segment times feed
        # the SPLIT deadlines and the heartbeat.
        output, per_layer_seconds = model.run(x, 0, stop_layer)
        segment_times.record(0, per_layer_seconds)
        return output, sum(per_layer_seconds) * 1000

    hb = HeartbeatLoop(
        ControlPlaneClient(cfg.server, device_id),
        segments.model_hash(model.dir),
        window,
        split_supported=True,
        probe=cfg.probe,
    )
    pipeline = SplitPipeline(
        server=cfg.server,
        device_id=device_id,
        heartbeat=hb,
        last_layer=last_layer,
        run_segments=model.run,
        segment_times=segment_times,
        outputs=[
            o
            for o in (
                None if args.no_table else FrameTable(),
                FrameCSV(args.frames_csv) if args.frames_csv else None,
            )
            if o is not None
        ],
        deadline_ms=args.deadline_ms,
        window=window,
    )

    # Last thing before hb.start(): a camera that does not open stops the client
    # before it registers, and nothing built above is left holding it.
    try:
        camera = open_camera(cfg, model)
    except (frames.FrameError, ImportError) as exc:
        hint = ""
        if isinstance(exc, ImportError):
            hint = {
                "opencv": " (uv sync --extra opencv)",
                "picamera2": " (docs/RASPBERRY.md: python3-picamera2 from apt, "
                "venv with --system-site-packages)",
            }.get(cfg.camera, "")
        logger.error("cannot open the camera: %s%s", exc, hint)
        return 1

    if camera is None:
        # Static image: loaded once, ~0 ms per frame (numbers comparable with 3.8).
        image = model.load_input(cfg.image)

        def acquire():
            return image, utc_now(), None
    else:

        def acquire():
            frame, captured_at, captured_t = camera.read()
            # Preprocessing counts in acq_ms.
            return model.to_input(frame), captured_at, captured_t

    stop = threading.Event()
    hb.start()
    started = time.monotonic()
    completed = 0
    try:
        completed = run_loop(
            lambda: hb.policy,
            window,
            acquire,
            infer,
            last_layer=last_layer,
            stop=stop,
            max_cycles=args.cycles,
            before_cycle=pipeline.drain,
            before_acquire=pipeline.before_acquire,
            on_output=pipeline.handle_output,
        )
    except KeyboardInterrupt:
        logger.info("interrupted")
    finally:
        stop.set()
        pipeline.finish()  # results of the frames still in flight
        hb.stop()
        if camera is not None:
            camera.close()

    elapsed = time.monotonic() - started
    rate = completed / elapsed if elapsed > 0 else 0.0
    logger.info(
        "done: %d inferences in %.1f s (%.1f/s), policy=%s",
        completed,
        elapsed,
        rate,
        hb.policy,
    )
    if pipeline.counts:
        logger.info("frames: %s", dict(sorted(pipeline.counts.items())))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
