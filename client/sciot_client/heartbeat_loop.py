"""Background heartbeat for the v1 control plane.

``HeartbeatLoop`` owns the pairing state shared with the inference loop:

    inference loop                      heartbeat thread
    --------------                      ----------------
    policy = hb.policy   (read)         every interval:
    run inference                         stats = window.snapshot_and_reset()
    window.record(ms)    (write)          policy = client.heartbeat(...)
                                          apply it if its version is newer

The inference loop never talks to the server: it reads ``hb.policy`` before
each inference and records the inference time in the shared window. A new
policy therefore takes effect from the next inference (report §21).

Usage::

    window = InferenceWindow()
    hb = HeartbeatLoop(client, model_hash, window)
    hb.start()
    while running:
        policy = hb.policy
        t0 = time.perf_counter()
        ... run LOCAL or SPLIT at policy.split_point ...
        window.record((time.perf_counter() - t0) * 1000)
    hb.stop()
"""

from __future__ import annotations

import logging
import threading
import time
from collections.abc import Callable

from sciot_client.control_plane_client import (
    MODE_LOCAL,
    MODE_SPLIT,
    ControlPlaneClient,
    InferenceWindow,
    ModelNotFound,
    PairingError,
    Policy,
    Session,
    SessionExpired,
    Telemetry,
)

logger = logging.getLogger(__name__)

# Retry interval while not paired (server down, registration refused).
# Before the first registration the server has not told us its interval yet.
DEFAULT_RETRY_INTERVAL_S = 3.0

# How often a status line is logged. Failures are logged only on the
# transition into an outage, so without this line a long outage would be
# a single warning followed by silence.
DEFAULT_STATUS_INTERVAL_S = 30.0


def default_metrics() -> dict:
    """Optional metrics for the heartbeat, from psutil when available.

    ``psutil.cpu_percent(interval=None)`` returns the CPU usage since the
    previous call: called once per heartbeat, it is already the average over
    the window, consistent with the inference stats.
    """
    try:
        import psutil
    except ImportError:
        return {}
    return {
        "cpu_load": psutil.cpu_percent(interval=None) / 100.0,
        "memory_available_bytes": int(psutil.virtual_memory().available),
    }


class HeartbeatLoop:
    """Keeps the device paired and its execution policy up to date.

    Two pieces of state, deliberately separate:

    - ``_server_policy``: the last policy accepted from the server;
    - ``_fallback``: True while the server is unreachable or the session
      expired. The effective policy is then LOCAL (report §19).

    Why not just overwrite the policy with LOCAL on failure? Because the
    server would answer again with the SAME policy_version (e.g. v2 SPLIT),
    and the "apply only if newer" rule would ignore it forever: the device
    would stay LOCAL after the server came back.

    ``tick()`` performs one step synchronously and is what the tests drive;
    ``start()`` simply calls it from a background thread.

    Logging follows the server: events, not samples. A failed heartbeat or
    registration logs a warning only when an outage STARTS, and an info line
    when it ends; repeated failures in between go to debug. Every
    ``status_interval_s`` a status line summarises the current state.
    """

    def __init__(
        self,
        client: ControlPlaneClient,
        model_hash: str,
        window: InferenceWindow,
        *,
        metrics: Callable[[], dict] = default_metrics,
        clock: Callable[[], float] = time.monotonic,
        retry_interval_s: float = DEFAULT_RETRY_INTERVAL_S,
        status_interval_s: float = DEFAULT_STATUS_INTERVAL_S,
        split_supported: bool = False,
        probe: bool = False,
    ):
        self.client = client
        self.model_hash = model_hash
        self.window = window
        self.metrics = metrics
        self.clock = clock
        self.retry_interval_s = retry_interval_s
        self.status_interval_s = status_interval_s
        # Without a data plane the client cannot execute SPLIT: such a policy
        # is accepted (its version is tracked) but downgraded to LOCAL.
        self.split_supported = split_supported
        self.probe = probe  # capability declared at every registration

        # _lock protects the three fields below: they are written by the
        # heartbeat thread and read by the inference loop (via .policy).
        self._lock = threading.Lock()
        self._session: Session | None = None
        self._server_policy: Policy | None = None
        self._fallback = True  # LOCAL until the first successful pairing

        # Only touched by the heartbeat thread: no lock needed.
        self._last_rtt_ms: float | None = None
        self.local_only = False  # set when the server does not know the model
        self._outage_since: float | None = None  # clock() at the first failure
        self._outage_failures = 0  # failed calls in the current outage
        self._last_status_at: float | None = None

        self._stop = threading.Event()
        self._thread: threading.Thread | None = None

    # --- state read by the inference loop --------------------------------

    @property
    def policy(self) -> Policy:
        """The policy the inference loop must apply right now."""
        with self._lock:
            server_policy = self._server_policy
            # A SPLIT policy we cannot execute is downgraded like a fallback:
            # the heartbeat sends self.policy as acknowledgement, so the server
            # sees that its SPLIT policy is not being applied.
            unsupported = (
                server_policy is not None
                and server_policy.mode == MODE_SPLIT
                and not self.split_supported
            )
            if server_policy is None or self._fallback or unsupported:
                # Keep the server policy's version: in the acknowledgement it
                # tells the server which policy we are NOT applying.
                version = server_policy.policy_version if server_policy else 0
                return Policy(MODE_LOCAL, None, version)
            return server_policy

    @property
    def session(self) -> Session | None:
        with self._lock:
            return self._session

    @property
    def interval_s(self) -> float:
        """Seconds until the next tick: the server's interval once paired."""
        session = self.session
        if session is None:
            return self.retry_interval_s
        return session.heartbeat_interval_ms / 1000.0

    # --- one step ----------------------------------------------------------

    def tick(self) -> None:
        """Register if needed, otherwise send one heartbeat."""
        if not self.local_only:
            if self.session is None:
                self._register()
            else:
                self._heartbeat()
        self._maybe_log_status()

    def _register(self) -> None:
        try:
            session = self.client.register(self.model_hash, probe=self.probe)
        except ModelNotFound:
            logger.warning("server does not know model %s: LOCAL-only", self.model_hash)
            self.local_only = True
            return
        except PairingError as exc:
            self._outage("registration failed", exc)
            return
        with self._lock:
            self._session = session
            self._server_policy = session.policy
            self._fallback = False
        self._recovered()
        self.window.snapshot_and_reset()  # start the first window clean
        logger.info("paired: session=%s policy=%s", session.session_id, session.policy)

    def _heartbeat(self) -> None:
        stats = self.window.snapshot_and_reset()
        # rtt_ms is the duration of the PREVIOUS heartbeat: the current one is
        # still being measured.
        session = self.session
        telemetry = Telemetry(
            stats,
            rtt_ms=self._last_rtt_ms,
            num_segments=session.model.num_layers if session else None,
            **self.metrics(),
        )
        # The EFFECTIVE policy: during a fallback the server receives LOCAL as
        # the applied mode (acknowledgement).
        applied = self.policy

        start = self.clock()
        try:
            new_policy = self.client.heartbeat(session, applied, telemetry)
        except SessionExpired as exc:
            # Must come before PairingError, of which it is a subclass.
            with self._lock:
                self._session = None
                self._fallback = True
            # A 409 is an answer: the server is reachable again, even though
            # it no longer knows us.
            self._recovered()
            logger.warning("session expired, re-registering: %s", exc)
            return
        except PairingError as exc:
            # A failed call has no RTT: do not report the previous one as if
            # the network were fine.
            self._last_rtt_ms = None
            with self._lock:
                self._fallback = True
            self._outage("heartbeat failed, falling back to LOCAL", exc)
            return

        self._last_rtt_ms = (self.clock() - start) * 1000
        with self._lock:
            self._fallback = False
        self._recovered()
        self._apply(new_policy)

    def offer_policy(self, new_policy: Policy) -> None:
        """A policy from another channel (the data plane stream's ``policy``
        message): adopted only if newer, like a heartbeat answer. It is the
        fast path; the heartbeat stays the reliable one."""
        self._apply(new_policy)

    def _apply(self, new_policy: Policy) -> None:
        """Adopt new_policy if it is newer than the current server policy."""
        with self._lock:
            old = self._server_policy
            if old is not None and new_policy.policy_version <= old.policy_version:
                return  # same policy, or a late/out-of-order answer
            self._server_policy = new_policy
        # Outside the lock: discard the samples of the old policy, a window
        # must not mix LOCAL and SPLIT.
        self.window.snapshot_and_reset()
        logger.info("policy changed: %s -> %s", old, new_policy)
        # _apply runs only when the version changes, so this warns once per
        # policy, not at every heartbeat.
        if new_policy.mode == MODE_SPLIT and not self.split_supported:
            logger.warning(
                "server asked for SPLIT but this client has no data plane: running LOCAL"
            )

    # --- logging -------------------------------------------------------------

    def _outage(self, what: str, exc: Exception) -> None:
        """Record a failed call; warn only on the first one of an outage."""
        self._outage_failures += 1
        if self._outage_since is None:
            self._outage_since = self.clock()
            logger.warning("%s: %s", what, exc)
        else:
            logger.debug("%s (failure %d): %s", what, self._outage_failures, exc)

    def _recovered(self) -> None:
        """Close the current outage, if any, with a single info line."""
        if self._outage_since is None:
            return
        logger.info(
            "server reachable again after %.1fs (%d failed attempts)",
            self.clock() - self._outage_since,
            self._outage_failures,
        )
        self._outage_since = None
        self._outage_failures = 0

    def _maybe_log_status(self) -> None:
        """Log a status line every status_interval_s (not on the first tick)."""
        now = self.clock()
        if self._last_status_at is None:
            self._last_status_at = now
            return
        if now - self._last_status_at < self.status_interval_s:
            return
        self._last_status_at = now

        session = self.session
        if self.local_only:
            state = "LOCAL-only (model unknown to the server)"
        elif self._outage_since is not None:
            state = (
                f"outage for {now - self._outage_since:.0f}s "
                f"({self._outage_failures} failed attempts)"
            )
        else:
            state = "ok"
        rtt = f"{self._last_rtt_ms:.1f}ms" if self._last_rtt_ms is not None else "n/a"
        logger.info(
            "status: %s, session=%s, policy=%s, rtt=%s",
            state,
            session.session_id if session else None,
            self.policy,
            rtt,
        )

    # --- background thread -------------------------------------------------

    def start(self) -> None:
        """Run tick() every interval_s in a daemon thread."""
        if self._thread is not None:
            raise RuntimeError("HeartbeatLoop already started")
        # daemon=True: the thread does not keep the process alive on exit.
        self._thread = threading.Thread(target=self._run, name="heartbeat", daemon=True)
        self._thread.start()

    def stop(self, timeout: float = 5.0) -> None:
        """Ask the thread to stop and wait for it."""
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout)

    def _run(self) -> None:
        # First tick immediately (registration at startup), then wait.
        while not self._stop.is_set():
            try:
                self.tick()
            except Exception:
                # An unexpected bug must not kill the thread silently and leave
                # the device without heartbeats.
                logger.exception("heartbeat tick failed")
            # Unlike time.sleep, wait() returns as soon as stop() is called.
            self._stop.wait(self.interval_s)
