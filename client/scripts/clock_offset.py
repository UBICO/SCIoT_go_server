"""Clock offset between a device and this machine (the server's), over ssh.

The analysis matches the client's CSV (the device's wall clock) with the
server's recording (this machine's wall clock), docs/RASPBERRY.md "Clocks".
One ssh session stays open and prints the device's time on request; of
``--samples`` round trips the shortest wins: the offset is the device's time
minus the midpoint of the round trip, within half its duration.

Example (from the server's machine)::

    python3 client/scripts/clock_offset.py <user>@<pi>
"""

from __future__ import annotations

import argparse
import subprocess
import time


def best_offset(samples: list[tuple[float, float, float]]) -> tuple[float, float]:
    """(offset, uncertainty) in seconds from (sent, device time, received)
    samples on this machine's clock: the sample with the shortest round trip."""
    a, t, b = min(samples, key=lambda s: s[2] - s[0])
    return t - (a + b) / 2, (b - a) / 2


def main(argv: list[str] | None = None) -> None:
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("target", help="ssh target of the device, e.g. pi@raspberrypi")
    p.add_argument("--samples", type=int, default=20)
    args = p.parse_args(argv)
    proc = subprocess.Popen(
        ["ssh", args.target, "while read l; do date +%s.%N; done"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        text=True,
    )
    assert proc.stdin is not None and proc.stdout is not None
    samples = []
    try:
        for _ in range(args.samples):
            a = time.time()
            proc.stdin.write("x\n")
            proc.stdin.flush()
            line = proc.stdout.readline()
            b = time.time()
            if not line:
                raise SystemExit(f"ssh {args.target} closed (exit {proc.wait()})")
            samples.append((a, float(line), b))
            time.sleep(0.2)
    finally:
        proc.terminate()
    offset, err = best_offset(samples)
    print(f"offset {args.target} - this machine: {offset * 1000:+.1f} ms (±{err * 1000:.1f})")


if __name__ == "__main__":
    main()
