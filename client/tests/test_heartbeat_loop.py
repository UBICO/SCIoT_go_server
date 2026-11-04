import threading

import pytest

from sciot_client import control_plane_client as cpc
from sciot_client.heartbeat_loop import HeartbeatLoop

MODEL = cpc.ModelInfo(
    "fomo_96x96",
    59,
    58,
    cpc.TensorSpec((1, 96, 96, 3), "float32"),
    cpc.Postprocess("fomo", ("bicycle", "stop sign"), 0.8, 10, None),
)
LOCAL_V1 = cpc.Policy("LOCAL", None, 1)
SPLIT_V2 = cpc.Policy("SPLIT", 5, 2)


def session(sid="S1", interval_ms=1000):
    return cpc.Session(sid, MODEL, LOCAL_V1, interval_ms)


class FakeClient:
    """Scripted ControlPlaneClient: each call pops the next outcome.

    An outcome is either a value to return or an exception to raise.
    """

    def __init__(self, registers=(), heartbeats=()):
        self.registers = list(registers)
        self.heartbeats = list(heartbeats)
        self.register_calls = []
        self.probe_flags = []
        self.heartbeat_calls = []

    @staticmethod
    def _next(outcomes):
        outcome = outcomes.pop(0)
        if isinstance(outcome, Exception):
            raise outcome
        return outcome

    def register(self, model_hash, *, probe=False):
        self.register_calls.append(model_hash)
        self.probe_flags.append(probe)
        return self._next(self.registers)

    def heartbeat(self, session, applied, telemetry):
        self.heartbeat_calls.append(
            {"session": session, "applied": applied, "telemetry": telemetry}
        )
        return self._next(self.heartbeats)


class FakeClock:
    """Every call advances by `step` seconds: a heartbeat "lasts" step s."""

    def __init__(self, step=0.004):
        self.now = 100.0
        self.step = step

    def __call__(self):
        self.now += self.step
        return self.now


def make_loop(client, window=None, **kwargs):
    kwargs.setdefault("metrics", lambda: {"cpu_load": 0.25})
    kwargs.setdefault("clock", FakeClock())
    return HeartbeatLoop(client, "HASH", window or cpc.InferenceWindow(), **kwargs)


def unreachable():
    return cpc.ServerUnreachable("down")


def expired():
    return cpc.SessionExpired(409, "session_expired", "session expired")


# --- before pairing ------------------------------------------------------------


def test_local_before_pairing():
    loop = make_loop(FakeClient())
    assert loop.policy == cpc.Policy("LOCAL", None, 0)
    assert loop.session is None
    assert loop.interval_s == loop.retry_interval_s


def test_first_tick_registers():
    client = FakeClient(registers=[session(interval_ms=500)])
    loop = make_loop(client)

    loop.tick()

    assert client.register_calls == ["HASH"]
    assert loop.session.session_id == "S1"
    assert loop.policy == LOCAL_V1
    assert loop.interval_s == 0.5


def test_registration_declares_the_probe_capability():
    for probe in (True, False):
        client = FakeClient(registers=[session()])
        make_loop(client, probe=probe).tick()
        assert client.probe_flags == [probe]


def test_registration_retry_after_failure():
    client = FakeClient(registers=[unreachable(), session()])
    loop = make_loop(client)

    loop.tick()
    assert loop.session is None and loop.policy.mode == cpc.MODE_LOCAL
    loop.tick()
    assert loop.session is not None


def test_model_not_found_is_local_only():
    nf = cpc.ModelNotFound(404, "model_not_found", "unknown")
    client = FakeClient(registers=[nf])
    loop = make_loop(client)

    loop.tick()
    loop.tick()  # must not call register again (the script is empty)

    assert loop.local_only
    assert len(client.register_calls) == 1
    assert loop.policy.mode == cpc.MODE_LOCAL


# --- heartbeat -----------------------------------------------------------------


def paired_loop(heartbeats, window=None, **kwargs):
    client = FakeClient(registers=[session()], heartbeats=heartbeats)
    loop = make_loop(client, window, **kwargs)
    loop.tick()  # registration
    return loop, client


def test_heartbeat_sends_window_and_ack():
    window = cpc.InferenceWindow()
    loop, client = paired_loop([LOCAL_V1, LOCAL_V1], window)

    for ms in (10.0, 30.0):
        window.record(ms)
    loop.tick()

    (call,) = client.heartbeat_calls
    assert call["session"].session_id == "S1"
    assert call["applied"] == LOCAL_V1
    t = call["telemetry"]
    assert t.window == cpc.WindowStats(2, 20.0, 30.0)
    assert t.cpu_load == 0.25
    assert t.rtt_ms is None  # no previous heartbeat yet
    assert t.num_segments == MODEL.num_layers  # segment_ms sized on the model

    loop.tick()
    # The second heartbeat carries the RTT of the first (FakeClock: 4 ms).
    assert client.heartbeat_calls[1]["telemetry"].rtt_ms == pytest.approx(4.0)
    assert client.heartbeat_calls[1]["telemetry"].window.count == 0


def test_newer_policy_is_applied():
    loop, _ = paired_loop([SPLIT_V2], split_supported=True)
    loop.tick()
    assert loop.policy == SPLIT_V2


def test_same_or_older_policy_is_ignored():
    stale = cpc.Policy("LOCAL", None, 1)
    loop, _ = paired_loop([SPLIT_V2, stale], split_supported=True)
    loop.tick()
    loop.tick()  # a late answer with an older version must not roll back
    assert loop.policy == SPLIT_V2


def test_policy_change_resets_window():
    window = cpc.InferenceWindow()
    loop, _ = paired_loop([SPLIT_V2], window)

    loop.tick()  # snapshot (empty), then SPLIT_V2 is applied
    window.record(12.0)  # recorded under SPLIT: must survive
    assert window.snapshot_and_reset().count == 1


def test_policy_change_discards_old_samples():
    window = cpc.InferenceWindow()
    client = FakeClient(registers=[session()])
    loop = make_loop(client, window)
    loop.tick()

    # Simulate inferences recorded under LOCAL while the heartbeat that
    # brings SPLIT is in flight: they must be discarded on the change.
    def heartbeat_with_inflight_samples(session, applied, telemetry):
        window.record(99.0)
        return SPLIT_V2

    client.heartbeat = heartbeat_with_inflight_samples
    loop.tick()
    assert window.snapshot_and_reset().count == 0


def test_session_expired_falls_back_and_re_registers():
    client = FakeClient(
        registers=[session("S1"), session("S2")],
        heartbeats=[SPLIT_V2, expired()],
    )
    loop = make_loop(client)
    loop.tick()  # register S1
    loop.tick()  # SPLIT_V2
    loop.tick()  # 409

    assert loop.session is None
    assert loop.policy.mode == cpc.MODE_LOCAL

    loop.tick()  # register again
    assert loop.session.session_id == "S2"
    assert loop.policy == LOCAL_V1  # the new session starts from its own policy


def test_unreachable_falls_back_and_recovers_same_version():
    # The pitfall described in the HeartbeatLoop docstring: after the server
    # comes back with the SAME policy version, the device must leave LOCAL.
    loop, client = paired_loop([SPLIT_V2, unreachable(), SPLIT_V2], split_supported=True)
    loop.tick()
    assert loop.policy == SPLIT_V2

    loop.tick()  # server down
    assert loop.policy == cpc.Policy("LOCAL", None, 2)
    assert loop.session is not None  # session kept: no re-registration

    loop.tick()  # server back, same v2
    assert loop.policy == SPLIT_V2
    # During the fallback the server was told the truth: LOCAL is applied.
    assert client.heartbeat_calls[2]["applied"] == cpc.Policy("LOCAL", None, 2)


def test_rtt_not_reported_after_failure():
    loop, client = paired_loop([LOCAL_V1, unreachable(), LOCAL_V1])
    loop.tick()  # ok: RTT measured
    loop.tick()  # failed
    loop.tick()
    # The third heartbeat must not carry the RTT measured before the failure.
    assert client.heartbeat_calls[2]["telemetry"].rtt_ms is None


def test_split_downgraded_when_unsupported():
    loop, client = paired_loop([SPLIT_V2, SPLIT_V2])
    loop.tick()
    # Accepted (version tracked) but executed as LOCAL...
    assert loop.policy == cpc.Policy("LOCAL", None, 2)
    loop.tick()
    # ...and the server is told so in the acknowledgement.
    assert client.heartbeat_calls[1]["applied"] == cpc.Policy("LOCAL", None, 2)


def test_split_applied_when_supported():
    loop, _ = paired_loop([SPLIT_V2], split_supported=True)
    loop.tick()
    assert loop.policy == SPLIT_V2


def test_split_unsupported_warns_once(caplog):
    loop, _ = paired_loop([SPLIT_V2, SPLIT_V2, SPLIT_V2])
    with caplog.at_level("WARNING", logger="sciot_client.heartbeat_loop"):
        for _ in range(3):
            loop.tick()
    warnings = [r for r in caplog.records if r.levelname == "WARNING" and "SPLIT" in r.getMessage()]
    assert len(warnings) == 1


def test_policy_read_from_another_thread():
    loop, _ = paired_loop([SPLIT_V2] + [SPLIT_V2] * 200)
    errors = []

    def reader():
        for _ in range(2000):
            if loop.policy.mode not in (cpc.MODE_LOCAL, cpc.MODE_SPLIT):
                errors.append("invalid")

    t = threading.Thread(target=reader)
    t.start()
    for _ in range(201):
        loop.tick()
    t.join()
    assert not errors


# --- background thread ---------------------------------------------------------


def test_thread_registers_and_stops_promptly():
    registered = threading.Event()

    class SignallingClient(FakeClient):
        def register(self, model_hash, *, probe=False):
            s = super().register(model_hash, probe=probe)
            registered.set()
            return s

        def heartbeat(self, session, applied, telemetry):
            return LOCAL_V1

    client = SignallingClient(registers=[session(interval_ms=60_000)])
    loop = make_loop(client, clock=lambda: 0.0)

    loop.start()
    assert registered.wait(2.0), "the first tick must register immediately"
    loop.stop(timeout=2.0)  # interval is 60 s: stop must not wait for it

    assert not loop._thread.is_alive()


def test_thread_survives_unexpected_errors():
    calls = threading.Event()
    count = {"n": 0}

    class BuggyClient(FakeClient):
        def register(self, model_hash, *, probe=False):
            count["n"] += 1
            if count["n"] >= 2:
                calls.set()
            raise RuntimeError("bug")  # not a PairingError

    loop = make_loop(BuggyClient(), retry_interval_s=0.01)
    loop.start()
    assert calls.wait(2.0), "the thread must keep ticking after an exception"
    loop.stop()


# --- logging -------------------------------------------------------------------


class ManualClock:
    """A clock that moves only when the test says so."""

    def __init__(self):
        self.now = 0.0

    def __call__(self):
        return self.now


LOGGER = "sciot_client.heartbeat_loop"


def messages(caplog, level):
    return [r.getMessage() for r in caplog.records if r.levelname == level]


def test_outage_logs_only_transitions(caplog):
    caplog.set_level("DEBUG", logger=LOGGER)
    clock = ManualClock()
    loop, _ = paired_loop([unreachable(), unreachable(), unreachable(), LOCAL_V1], clock=clock)
    caplog.clear()

    for _ in range(3):  # three failed heartbeats, 1 s apart
        loop.tick()
        clock.now += 1.0
    loop.tick()  # server back

    warnings = messages(caplog, "WARNING")
    assert len(warnings) == 1, warnings  # only the start of the outage
    assert "heartbeat failed" in warnings[0]
    assert len(messages(caplog, "DEBUG")) == 2  # the repeated failures
    infos = messages(caplog, "INFO")
    assert any("reachable again after 3.0s (3 failed attempts)" in m for m in infos)


def test_registration_retries_log_once(caplog):
    caplog.set_level("DEBUG", logger=LOGGER)
    client = FakeClient(registers=[unreachable()] * 5 + [session()])
    loop = make_loop(client, clock=ManualClock())

    for _ in range(6):
        loop.tick()

    assert len(messages(caplog, "WARNING")) == 1
    assert any("5 failed attempts" in m for m in messages(caplog, "INFO"))
    assert loop.session is not None


def test_new_outage_after_recovery_warns_again(caplog):
    caplog.set_level("DEBUG", logger=LOGGER)
    loop, _ = paired_loop([unreachable(), LOCAL_V1, unreachable()], clock=ManualClock())
    caplog.clear()
    for _ in range(3):
        loop.tick()
    assert len(messages(caplog, "WARNING")) == 2  # two distinct outages


def test_status_logged_every_interval(caplog):
    caplog.set_level("INFO", logger=LOGGER)
    clock = ManualClock()
    loop, _ = paired_loop([LOCAL_V1] * 10, clock=clock, status_interval_s=30.0)

    statuses = []
    for t in (10, 29, 31, 45, 60, 62):
        clock.now = t
        caplog.clear()
        loop.tick()
        statuses += [m for m in messages(caplog, "INFO") if m.startswith("status:")]

    # Reference set at t=0 (registration tick): status at 31 and then at 62.
    assert len(statuses) == 2, statuses
    assert "status: ok" in statuses[0]
    assert "session=S1" in statuses[0]


def test_status_during_outage(caplog):
    caplog.set_level("INFO", logger=LOGGER)
    clock = ManualClock()
    loop, _ = paired_loop([unreachable()] * 10, clock=clock, status_interval_s=30.0)

    clock.now = 5
    loop.tick()  # outage starts at t=5
    caplog.clear()
    clock.now = 35
    loop.tick()

    (status,) = [m for m in messages(caplog, "INFO") if m.startswith("status:")]
    assert "outage for 30s (2 failed attempts)" in status
    assert "mode='LOCAL'" in status


def test_session_expired_closes_outage(caplog):
    caplog.set_level("INFO", logger=LOGGER)
    loop, _ = paired_loop([unreachable(), expired()], clock=ManualClock())
    caplog.clear()
    loop.tick()  # outage
    loop.tick()  # 409: the server answered

    infos = messages(caplog, "INFO")
    warnings = messages(caplog, "WARNING")
    assert any("reachable again" in m for m in infos)
    # Recovery is logged before the session-expired warning, not after the
    # next registration.
    order = [r.getMessage() for r in caplog.records]
    recovered = next(i for i, m in enumerate(order) if "reachable again" in m)
    expired_at = next(i for i, m in enumerate(order) if "session expired" in m)
    assert recovered < expired_at
    assert len(warnings) == 2  # outage start + session expired


# --- against a running Go server ---------------------------------------------


@pytest.mark.external_server
@pytest.mark.parametrize("split_supported", [False, True])
def test_policy_change_with_go_server(split_supported):
    """Admin-forced policy reaches a real HeartbeatLoop.

    Run with the Go server started with -admin and its model directory:
        SCIOT_GO_SERVER_URL=http://127.0.0.1:8080 SCIOT_MODEL_DIR=~/sciot-models/yolo_small_320 \
            uv run --project client pytest -m external_server
    """
    import json
    import os
    import urllib.error
    import urllib.request

    from sciot_client import segments

    url, model_dir = os.environ.get("SCIOT_GO_SERVER_URL"), os.environ.get("SCIOT_MODEL_DIR")
    if not url or not model_dir:
        pytest.skip("SCIOT_GO_SERVER_URL or SCIOT_MODEL_DIR not set")
    device_id = f"pytest-policy-{split_supported}"

    def set_policy(body):
        request = urllib.request.Request(
            f"{url}/api/v1/admin/devices/{device_id}/policy",
            data=json.dumps(body).encode(),
            method="POST",
        )
        try:
            with urllib.request.urlopen(request, timeout=5) as response:
                return json.load(response)["policy"]
        except urllib.error.HTTPError as exc:
            if exc.code == 404 and "device_not_found" not in exc.read().decode():
                pytest.skip("admin endpoint not enabled (start the server with -admin)")
            raise

    loop = HeartbeatLoop(
        cpc.ControlPlaneClient(url, device_id),
        segments.model_hash(os.path.expanduser(model_dir)),
        cpc.InferenceWindow(),
        metrics=dict,
        split_supported=split_supported,
    )
    loop.tick()  # register
    assert loop.policy == cpc.Policy("LOCAL", None, 1)

    assert set_policy({"mode": "SPLIT", "split_point": 5}) == {
        "mode": "SPLIT",
        "split_point": 5,
        "policy_version": 2,
    }
    loop.tick()  # heartbeat brings SPLIT v2
    expected = cpc.Policy("SPLIT", 5, 2) if split_supported else cpc.Policy("LOCAL", None, 2)
    assert loop.policy == expected

    set_policy({"mode": "LOCAL", "split_point": None})
    loop.tick()
    assert loop.policy == cpc.Policy("LOCAL", None, 3)
