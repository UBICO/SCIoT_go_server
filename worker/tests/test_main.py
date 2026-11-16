import signal
import subprocess
import sys
import time

# Phase 4.2: a burst of SIGTERMs (the Go server's to the group, plus the one
# `uv run` forwards) deadlocked the worker when the handler set a
# threading.Event; wait_for_stop must return whatever the burst.
WAIT = "from sciot_worker.__main__ import wait_for_stop; print('ready', flush=True); wait_for_stop(0.05)"


def test_wait_for_stop_survives_a_burst_of_signals():
    for _ in range(10):
        proc = subprocess.Popen([sys.executable, "-c", WAIT], stdout=subprocess.PIPE, text=True)
        try:
            assert proc.stdout.readline().strip() == "ready"
            for _ in range(8):
                proc.send_signal(signal.SIGTERM)
            assert proc.wait(timeout=3) == 0
        finally:
            proc.kill()
            proc.wait()


def test_wait_for_stop_on_sigint():
    proc = subprocess.Popen([sys.executable, "-c", WAIT], stdout=subprocess.PIPE, text=True)
    try:
        assert proc.stdout.readline().strip() == "ready"
        time.sleep(0.1)
        proc.send_signal(signal.SIGINT)
        assert proc.wait(timeout=3) == 0
    finally:
        proc.kill()
        proc.wait()
