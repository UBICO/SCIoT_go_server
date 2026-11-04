"""The helpers of scripts/bench_common.py."""

import socket
import subprocess
import sys
import threading
import time

import pytest

from scripts.bench_common import CountingProxy, summarize, wait_http_ok


def test_summarize():
    s = summarize([1.0, 2.0, 3.0])
    assert s["mean"] == 2.0 and s["min"] == 1.0 and s["max"] == 3.0 and s["n"] == 3
    assert s["stdev"] == pytest.approx(1.0)
    assert summarize([5.0])["stdev"] == 0.0
    with pytest.raises(ValueError):
        summarize([])


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def test_proxy_counts_both_directions():
    # Echo server that answers with twice the received bytes.
    server = socket.socket()
    server.bind(("127.0.0.1", 0))
    server.listen()
    target_port = server.getsockname()[1]

    def serve():
        conn, _ = server.accept()
        with conn:
            data = conn.recv(1024)
            conn.sendall(data * 2)

    threading.Thread(target=serve, daemon=True).start()
    proxy = CountingProxy(free_port(), "127.0.0.1", target_port).start()
    try:
        with socket.create_connection(("127.0.0.1", proxy.listen_port)) as c:
            c.sendall(b"hello")
            received = b""
            while len(received) < 10:
                received += c.recv(1024)
        assert received == b"hellohello"
        # Counters are updated by the proxy thread: wait briefly.
        for _ in range(50):
            if proxy.counts() == (5, 10):
                break
            threading.Event().wait(0.02)
        assert proxy.counts() == (5, 10)

        proxy.reset()
        assert proxy.counts() == (0, 0)
    finally:
        proxy.stop()
        server.close()


def test_wait_fails_fast_when_server_exits(tmp_path):
    # A "server" that crashes at import, like the Python server without fastapi.
    log = tmp_path / "server.log"
    with log.open("w") as out:
        proc = subprocess.Popen(
            [
                sys.executable,
                "-c",
                "import sys; print('No module named fastapi'); sys.exit(1)",
            ],
            stdout=out,
            stderr=subprocess.STDOUT,
        )
    started = time.monotonic()
    with pytest.raises(RuntimeError) as info:
        wait_http_ok(f"http://127.0.0.1:{free_port()}/", 30, proc=proc, log_path=log)

    assert time.monotonic() - started < 10  # not the whole 30 s timeout
    assert "exited with code 1" in str(info.value)
    assert "No module named fastapi" in str(info.value)  # the log tail is shown
