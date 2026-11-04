"""Helpers shared by the client benchmarks (bench_split.py, bench_network.py).

Run the benchmarks from the client project, which has the client installed::

    uv run --project client python client/scripts/bench_split.py --model-dir ~/sciot-models/yolo_small_320
"""

from __future__ import annotations

import asyncio
import os
import statistics
import subprocess
import threading
import time
import urllib.request
from pathlib import Path

CLIENT_ROOT = Path(__file__).resolve().parents[1]
# The Go server repository this client lives in: built and started by the benchmarks.
GO_REPO = CLIENT_ROOT.parent
# Results are kept outside the system temporary directory, which macOS cleans.
RESULTS_DIR = Path(os.environ.get("SCIOT_RESULTS_DIR", CLIENT_ROOT / "results"))


def summarize(values: list[float]) -> dict:
    """Mean, sample stdev (0 with one value), min and max."""
    if not values:
        raise ValueError("no values")
    return {
        "mean": statistics.fmean(values),
        "stdev": statistics.stdev(values) if len(values) > 1 else 0.0,
        "min": min(values),
        "max": max(values),
        "n": len(values),
    }


# ---------------------------------------------------------------------------
# Counting proxy
# ---------------------------------------------------------------------------


class CountingProxy:
    """TCP proxy on 127.0.0.1:listen_port -> target, counting bytes.

    Runs its own asyncio loop in a daemon thread. ``reset()`` zeroes the
    counters between runs; ``counts()`` reads them.

    ``set_up_limit(bps)`` caps the upload (client → target) at ``bps`` bytes
    per second, shared by all connections like one link; None = no limit.
    """

    # With a limit, data is forwarded in small chunks at the link's pace: the
    # receiver sees bytes trickle in, not a burst every few hundred ms.
    LIMITED_CHUNK = 16 << 10
    # How far behind schedule a chunk may start (asyncio.sleep overshoots by
    # ~1 ms): late chunks catch up instead of lowering the rate.
    CATCH_UP_S = 0.005

    def __init__(self, listen_port: int, target_host: str, target_port: int):
        self.listen_port = listen_port
        self.target = (target_host, target_port)
        self._lock = threading.Lock()
        self._up = self._down = 0
        self._up_limit: float | None = None
        self._up_free_at = 0.0  # loop time when the link is free again
        self._loop = asyncio.new_event_loop()
        self._ready = threading.Event()
        self._thread = threading.Thread(target=self._serve, daemon=True)

    def start(self) -> CountingProxy:
        self._thread.start()
        if not self._ready.wait(5):
            raise RuntimeError("proxy did not start")
        return self

    def stop(self) -> None:
        self._loop.call_soon_threadsafe(self._loop.stop)
        self._thread.join(5)

    def reset(self) -> None:
        with self._lock:
            self._up = self._down = 0

    def counts(self) -> tuple[int, int]:
        with self._lock:
            return self._up, self._down

    def set_up_limit(self, bytes_per_s: float | None) -> None:
        """Cap the upload; takes effect from the next chunk."""
        with self._lock:
            self._up_limit = bytes_per_s

    def _add(self, up: int = 0, down: int = 0) -> None:
        with self._lock:
            self._up += up
            self._down += down

    async def _throttle(self, n: int) -> None:
        """Wait until n bytes have crossed the limited link (upload only).

        Every connection runs on this loop's thread: _up_free_at needs no lock.
        """
        with self._lock:
            limit = self._up_limit
        if limit is None:
            return
        now = self._loop.time()
        start = max(self._up_free_at, now - self.CATCH_UP_S)
        self._up_free_at = start + n / limit
        await asyncio.sleep(max(0.0, self._up_free_at - now))

    async def _pipe(self, reader, writer, direction: str) -> None:
        try:
            while True:
                limited = direction == "up" and self._up_limit is not None
                data = await reader.read(self.LIMITED_CHUNK if limited else 65536)
                if not data:
                    break
                if direction == "up":
                    await self._throttle(len(data))
                self._add(**{direction: len(data)})
                writer.write(data)
                await writer.drain()
        except (ConnectionError, OSError):
            pass
        finally:
            writer.close()

    async def _handle(self, client_reader, client_writer) -> None:
        try:
            server_reader, server_writer = await asyncio.open_connection(*self.target)
        except OSError:
            client_writer.close()
            return
        await asyncio.gather(
            self._pipe(client_reader, server_writer, "up"),
            self._pipe(server_reader, client_writer, "down"),
        )

    def _serve(self) -> None:
        asyncio.set_event_loop(self._loop)
        server = self._loop.run_until_complete(
            asyncio.start_server(self._handle, "127.0.0.1", self.listen_port)
        )
        self._ready.set()
        try:
            self._loop.run_forever()
        finally:
            server.close()
            # Cancel the connections still open (keep-alive) and let them
            # finish, so that closing the loop does not destroy pending tasks.
            pending = asyncio.all_tasks(self._loop)
            for task in pending:
                task.cancel()
            self._loop.run_until_complete(asyncio.gather(*pending, return_exceptions=True))
            self._loop.close()


# ---------------------------------------------------------------------------
# Processes
# ---------------------------------------------------------------------------


def log_tail(path: Path | None, lines: int = 20) -> str:
    """Last lines of a log file, for error messages."""
    if path is None or not path.exists():
        return ""
    tail = path.read_text(errors="replace").splitlines()[-lines:]
    return "\n".join("    " + line for line in tail)


def wait_http_ok(
    url: str,
    timeout_s: float,
    proc: subprocess.Popen | None = None,
    log_path: Path | None = None,
) -> None:
    """Poll url until it answers 200.

    Fails immediately if ``proc`` (the server) exits, instead of waiting for
    the whole timeout, and shows the end of its log: the log lives in a
    temporary directory that is deleted when the benchmark stops.
    """
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        if proc is not None and proc.poll() is not None:
            raise RuntimeError(
                f"server exited with code {proc.returncode} before answering {url}\n"
                f"{log_tail(log_path)}"
            )
        try:
            with urllib.request.urlopen(url, timeout=1) as response:
                if response.status == 200:
                    return
        except OSError:
            pass
        time.sleep(0.5)
    raise RuntimeError(f"server not ready after {timeout_s}s: {url}\n{log_tail(log_path)}")
