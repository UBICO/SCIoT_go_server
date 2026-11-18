"""scripts/clock_offset.py: the offset from the shortest round trip."""

from __future__ import annotations

import pytest

from scripts.clock_offset import best_offset


def test_best_offset_takes_the_shortest_round_trip():
    # (sent, device time, received): the device is 50 ms ahead; the slow
    # round trip (400 ms) would say +250 ms.
    samples = [(10.0, 10.65, 10.4), (20.0, 20.055, 20.01), (30.0, 30.06, 30.02)]
    offset, err = best_offset(samples)
    assert offset == pytest.approx(0.05)
    assert err == pytest.approx(0.005)
