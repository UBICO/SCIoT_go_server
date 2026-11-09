import threading
from types import SimpleNamespace

import pytest

from sciot_client import control_plane_client as cpc
from sciot_client import frames
from sciot_client.http_go_client import (
    open_camera,
    run_loop,
    stop_layer_for,
)

LAST = 58
LOCAL = cpc.Policy("LOCAL", None, 1)


# --- stop_layer_for ------------------------------------------------------------


def test_local_runs_whole_model():
    assert stop_layer_for(LOCAL, LAST) == LAST


# -1: nothing on the device (the model input goes to the server);
# LAST - 1: only the last segment is left to the server.
@pytest.mark.parametrize("split_point", [-1, 0, 5, LAST - 1])
def test_split_runs_up_to_split_point(split_point):
    assert stop_layer_for(cpc.Policy("SPLIT", split_point, 2), LAST) == split_point


# LAST would leave nothing to the server: it is LOCAL, not a SPLIT.
@pytest.mark.parametrize("split_point", [-2, LAST, LAST + 1])
def test_split_point_outside_model(split_point):
    with pytest.raises(ValueError):
        stop_layer_for(cpc.Policy("SPLIT", split_point, 2), LAST)


def test_unknown_mode():
    with pytest.raises(ValueError):
        stop_layer_for(cpc.Policy("REMOTE", None, 1), LAST)


# --- run_loop ------------------------------------------------------------------


class StepClock:
    """Each call advances by `step` seconds."""

    def __init__(self, step=0.005):
        self.now = 0.0
        self.step = step

    def __call__(self):
        self.now += self.step
        return self.now


def fixed_infer(ms=20.0, layers=None):
    """infer(sample, layer) that returns ("OUT<layer>", a fixed compute time)."""

    def infer(sample, layer):
        if layers is not None:
            layers.append(layer)
        return f"OUT{layer}", ms

    return infer


def loop(get_policy=lambda: LOCAL, **kwargs):
    kwargs.setdefault("window", cpc.InferenceWindow())
    kwargs.setdefault("acquire", lambda: ("IMG", "T", None))
    kwargs.setdefault("infer", fixed_infer())
    kwargs.setdefault("stop", threading.Event())
    kwargs.setdefault("clock", StepClock())
    window = kwargs.pop("window")
    acquire = kwargs.pop("acquire")
    infer = kwargs.pop("infer")
    return run_loop(get_policy, window, acquire, infer, last_layer=LAST, **kwargs)


def test_runs_max_cycles_and_records_compute_time():
    window = cpc.InferenceWindow()
    layers = []

    completed = loop(window=window, infer=fixed_infer(20.0, layers), max_cycles=5)

    assert completed == 5
    assert layers == [LAST] * 5
    # The window gets the compute time returned by infer, not the acquisition.
    assert window.snapshot_and_reset() == cpc.WindowStats(5, 20.0, 20.0)


def test_sample_from_acquire_is_passed_to_infer():
    samples = iter([("frame-1", "T1", None), ("frame-2", "T2", None)])
    seen = []

    def infer(sample, layer):
        seen.append(sample)
        return None, 1.0

    loop(acquire=lambda: next(samples), infer=infer, max_cycles=2)
    assert seen == ["frame-1", "frame-2"]


def test_on_cycle_reports_layer_acq_and_comp():
    rows = []
    # StepClock(0.005): the two clock() calls around acquire() are 5 ms apart.
    loop(
        infer=fixed_infer(12.5),
        max_cycles=2,
        clock=StepClock(0.005),
        on_cycle=lambda *row: rows.append(row),
    )
    assert rows == [(LAST, pytest.approx(5.0), 12.5)] * 2


def test_policy_read_once_per_cycle():
    policies = [LOCAL, cpc.Policy("SPLIT", 3, 2), LOCAL]
    calls = []

    def get_policy():
        calls.append(1)
        return policies[len(calls) - 1]

    layers = []
    loop(get_policy, infer=fixed_infer(layers=layers), max_cycles=3)
    assert layers == [LAST, 3, LAST]
    assert len(calls) == 3


def test_stops_when_event_is_set():
    stop = threading.Event()
    count = []

    def infer(sample, layer):
        count.append(layer)
        if len(count) == 4:
            stop.set()
        return None, 1.0

    assert loop(infer=infer, stop=stop) == 4


def test_already_stopped_runs_nothing():
    stop = threading.Event()
    stop.set()
    assert loop(acquire=lambda: pytest.fail("must not run"), stop=stop) == 0


@pytest.mark.parametrize("where", ["acquire", "infer"])
def test_errors_propagate(where):
    def broken(*args):
        raise RuntimeError(where)

    with pytest.raises(RuntimeError):
        loop(max_cycles=3, **{where: broken})


def test_on_output_gets_the_policy_and_the_output():
    split = cpc.Policy("SPLIT", 3, 2)
    seen = []
    loop(
        lambda: split,
        max_cycles=2,
        clock=StepClock(0.005),
        on_output=lambda *args: seen.append(args),
    )
    assert seen == [(split, 3, "OUT3", "T", pytest.approx(5.0), 20.0, 0.0)] * 2


def test_captured_at_comes_from_acquire():
    # A camera frame was captured before acquire() returned it: its time, not the loop's.
    times = iter([("IMG", "CAP1", None), ("IMG", "CAP2", None)])
    seen = []
    loop(
        lambda: cpc.Policy("SPLIT", 3, 2),
        acquire=lambda: next(times),
        max_cycles=2,
        on_output=lambda policy, layer, out, captured_at, *rest: seen.append(captured_at),
    )
    assert seen == ["CAP1", "CAP2"]


@pytest.mark.parametrize(
    ("captured_t", "age_ms"),
    [
        (None, 0.0),  # the static image: it did not exist before the call
        (0.002, 3.0),  # captured 3 ms before acquire() started (clock: 0.005)
        (0.008, -3.0),  # arrived 3 ms after: acquire() waited for it
    ],
)
def test_age_of_the_frame_when_acquire_started(captured_t, age_ms):
    seen = []
    loop(
        acquire=lambda: ("IMG", "T", captured_t),
        max_cycles=1,
        clock=StepClock(0.005),  # acquire() starts at 0.005, ends at 0.010
        on_output=lambda *args: seen.append(args[-1]),
    )
    assert seen == [pytest.approx(age_ms)]


def test_run_loop_has_no_wall_clock():
    # captured_at comes only from acquire(): no second clock to disagree with it.
    import inspect

    assert "wall_clock" not in inspect.signature(run_loop).parameters


# --- open_camera ---------------------------------------------------------------


class FakeModel:
    input_shape = (1, 240, 320, 3)  # [1, h, w, 3]
    resize_mode = "fit_shortest"


def cfg(camera, index=0):
    return SimpleNamespace(camera=camera, camera_index=index)


def test_open_camera_static_is_none():
    assert open_camera(cfg("static"), FakeModel()) is None


def test_open_camera_opencv(monkeypatch):
    monkeypatch.setattr(frames, "OpenCVCamera", lambda index: ("opencv", index))
    assert open_camera(cfg("opencv", 2), FakeModel()) == ("opencv", 2)


def test_open_camera_picamera2_gets_width_height(monkeypatch):
    monkeypatch.setattr(frames, "PiCamera", lambda *args: ("pi", *args))
    # (width, height), not the manifest order (h, w).
    assert open_camera(cfg("picamera2", 1), FakeModel()) == ("pi", (320, 240), "fit_shortest", 1)


def test_before_cycle_runs_first_every_cycle():
    order = []

    def infer(sample, layer):
        order.append("infer")
        return None, 1.0

    loop(infer=infer, before_cycle=lambda: order.append("drain"), max_cycles=2)
    assert order == ["drain", "infer", "drain", "infer"]
