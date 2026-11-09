import itertools
import queue
import threading
import time

import numpy as np
import pytest

from sciot_client import frames


class FakeCamera:
    """grab() returns (or raises) the items put in the queue, blocking like a camera."""

    def __init__(self):
        self.items: queue.Queue = queue.Queue()
        self.stored = threading.Semaphore(0)  # released at each captured_at taken
        self._times = itertools.count(1)

    def grab(self) -> np.ndarray:
        item = self.items.get()
        if isinstance(item, BaseException):
            raise item
        return item

    def clock(self) -> str:
        self.stored.release()
        return f"T{next(self._times)}"

    def wait_stored(self, n: int = 1) -> None:
        for _ in range(n):
            assert self.stored.acquire(timeout=2), "the thread stored no frame"
        time.sleep(0.02)  # let the thread finish storing


def frame(value: int) -> np.ndarray:
    return np.full((2, 2, 3), value, np.uint8)


@pytest.fixture
def camera():
    cams = []

    def make(**kwargs):
        fake = FakeCamera()
        kwargs.setdefault("timeout_s", 1.0)
        kwargs.setdefault("monotonic", itertools.count(100).__next__)
        latest = frames.LatestFrame(fake.grab, clock=fake.clock, **kwargs)
        cams.append((fake, latest))
        return fake, latest

    yield make
    for fake, latest in cams:
        fake.items.put(frame(0))  # unblock a grab() in progress
        latest.close()


def test_read_returns_each_new_frame_once(camera):
    fake, latest = camera()
    fake.items.put(frame(1))
    got, at, _ = latest.read()
    assert np.array_equal(got, frame(1)) and at == "T1"
    fake.items.put(frame(2))
    got, at, _ = latest.read()
    assert np.array_equal(got, frame(2)) and at == "T2"


def test_read_gives_the_monotonic_time_of_the_same_frame(camera):
    # captured_t is on run_loop's clock (perf_counter): it gives the frame's age.
    fake, latest = camera()
    fake.items.put(frame(1))
    assert latest.read()[1:] == ("T1", 100)
    fake.items.put(frame(2))
    assert latest.read()[1:] == ("T2", 101)


def test_read_skips_to_the_newest(camera):
    fake, latest = camera()
    for v in (1, 2, 3):
        fake.items.put(frame(v))
    fake.wait_stored(3)
    got, at, _ = latest.read()
    assert np.array_equal(got, frame(3)) and at == "T3"


def test_read_waits_for_a_frame_newer_than_the_last(camera):
    fake, latest = camera()
    fake.items.put(frame(1))
    latest.read()
    threading.Timer(0.05, fake.items.put, [frame(2)]).start()
    got, _, _ = latest.read()  # not frame 1 again
    assert np.array_equal(got, frame(2))


def test_no_new_frame_times_out(camera):
    fake, latest = camera(timeout_s=0.1)
    fake.items.put(frame(1))
    latest.read()
    with pytest.raises(frames.FrameError, match="no frame"):
        latest.read()


def test_transient_errors_are_retried(camera):
    fake, latest = camera(max_errors=3)
    for item in (RuntimeError("a"), RuntimeError("b"), frame(1)):
        fake.items.put(item)
    got, _, _ = latest.read()
    assert np.array_equal(got, frame(1))


def test_errors_count_only_in_a_row(camera):
    fake, latest = camera(max_errors=3)
    for item in (
        RuntimeError(),
        RuntimeError(),
        frame(1),
        RuntimeError(),
        RuntimeError(),
        frame(2),
    ):
        fake.items.put(item)
    fake.wait_stored(2)
    got, _, _ = latest.read()
    assert np.array_equal(got, frame(2))


def test_gives_up_after_max_errors(camera):
    fake, latest = camera(max_errors=3)
    for name in ("a", "b", "c"):
        fake.items.put(RuntimeError(name))
    with pytest.raises(frames.FrameError) as exc:
        latest.read()
    assert str(exc.value.__cause__) == "c"  # the last read error
    latest._thread.join(timeout=1)
    assert not latest._thread.is_alive()


def test_close_stops_the_thread(camera):
    fake, latest = camera()
    threading.Timer(0.05, fake.items.put, [frame(1)]).start()  # ends the grab() in progress
    latest.close()
    assert not latest._thread.is_alive()


# --- isp_config ---------------------------------------------------------------------------

CAM3 = (0, 0, 4608, 2592)  # Camera Module 3, 16:9
OFFSET_4_3 = (16, 8, 4000, 3000)  # a 4:3 sensor whose active area does not start at 0


@pytest.mark.parametrize(
    "crop_max, input_size, crop",
    [
        (CAM3, (320, 320), (1008, 0, 2592, 2592)),  # crop left and right
        (OFFSET_4_3, (320, 320), (16 + 500, 8, 3000, 3000)),  # offset by the area origin
        (OFFSET_4_3, (160, 320), (16 + 1250, 8, 1500, 3000)),  # a portrait input
        ((0, 0, 3000, 4000), (320, 320), (0, 500, 3000, 3000)),  # crop top and bottom
    ],
)
def test_fit_shortest_crops_the_center(crop_max, input_size, crop):
    assert frames.isp_config("fit_shortest", crop_max, input_size) == (crop, input_size)


@pytest.mark.parametrize("mode", ["squash", "none"])
def test_squash_and_none_use_the_whole_sensor(mode):
    assert frames.isp_config(mode, OFFSET_4_3, (320, 320)) == (OFFSET_4_3, (320, 320))


@pytest.mark.parametrize(
    "crop_max, size",
    [(CAM3, (320, 180)), (OFFSET_4_3, (320, 240)), ((0, 0, 3000, 4000), (240, 320))],
)
def test_fit_longest_keeps_the_sensor_aspect_ratio(crop_max, size):
    assert frames.isp_config("fit_longest", crop_max, (320, 320)) == (crop_max, size)


@pytest.mark.parametrize(
    "crop_max, input_size, size",
    [
        (OFFSET_4_3, (320, 160), (213, 160)),  # input wider than the sensor: fit the height
        (OFFSET_4_3, (160, 320), (160, 120)),  # input taller: fit the width
    ],
)
def test_fit_longest_fits_inside_a_non_square_input(crop_max, input_size, size):
    # Same sizes as the SDK and segments.to_model_input: never larger than the input.
    assert frames.isp_config("fit_longest", crop_max, input_size) == (crop_max, size)


def test_isp_config_rejects_an_unknown_mode():
    with pytest.raises(ValueError):
        frames.isp_config("crop", CAM3, (320, 320))
