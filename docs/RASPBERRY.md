# Raspberry Pi device

A step-by-step guide: the Python client ([CLIENT.md](CLIENT.md)) on a
Raspberry Pi, with or without its camera, against a Go server on another
machine over a real network, then the phase 3.9 benchmark.

In the commands, `<user>@<pi>` is the Pi's user and address (as for `ssh`),
`<ip>` the address of the machine running the server. Lines marked **check**
are facts not yet verified on a Pi: note what you see.

## 1. The Pi

- **64-bit OS**: Raspberry Pi OS 64-bit (Bookworm, Python 3.11, or Trixie,
  Python 3.13). TensorFlow 2.20 has wheels only for aarch64, and only for
  Python 3.11–3.13.

  ```bash
  uname -m                           # aarch64
  python3 --version                  # 3.11.x or 3.13.x
  cat /proc/device-tree/model; echo  # e.g. Raspberry Pi 5 Model B Rev 1.0
  free -h                            # TensorFlow + yolo-small: 2 GB is tight (check)
  ```

- **Camera** (skip without one) connected and seen by libcamera:

  ```bash
  rpicam-hello --list-cameras   # libcamera-hello on older images
  ```

## 2. Install

Picamera2 is a Raspberry Pi OS package, not on PyPI: with a camera, the
client's venv is built on the system Python with `--system-site-packages`, so
it sees it. Without a camera none of this is needed.

```bash
sudo apt update
sudo apt install -y python3-picamera2 rsync   # without a camera: rsync only
curl -LsSf https://astral.sh/uv/install.sh | sh   # then open a new shell
```

rsync creates only the last directory of the destination: create the parents
first. From the machine with this repository, at its root:

```bash
ssh <user>@<pi> 'mkdir -p ~/sciot ~/sciot-models'
rsync -av --exclude .venv --exclude results client/ <user>@<pi>:~/sciot/client/
```

(Or `git clone` this repository on the Pi, on the branch you want.) Run the
client and the server from the same commit: the server rejects telemetry
fields it does not know yet (`wait_ms` before phase 3.9, `answer_ms` before
phase 4.2). On the Pi:

```bash
cd ~/sciot/client
uv venv --system-site-packages --python /usr/bin/python3   # without a camera: uv venv --python /usr/bin/python3
uv sync --frozen
```

`uv sync` keeps the existing venv: `--python /usr/bin/python3` is the
interpreter Picamera2 was built for. No `--extra opencv`: the Pi does not use
OpenCV. `uv sync` downloads TensorFlow (about 250 MB): a few minutes.

**Check numpy.** The venv's numpy (from PyPI, 2.x) hides the apt one, and
Picamera2's compiled dependencies (e.g. `simplejpeg`) were built against the
apt numpy. Check that both import (without a camera, drop `picamera2`):

```bash
uv run python -c "import numpy, picamera2, tensorflow as tf; print(numpy.__version__, tf.__version__)"
```

Verified with numpy 2.5.4, TensorFlow 2.20 and the apt Picamera2 (Raspberry
Pi OS, 2026-11-09). An error like `numpy.dtype size changed` or `module
compiled against ABI version` means a mismatch. Possible fixes, to try in this
order: the PyPI build of the failing module in the venv (`uv pip install
simplejpeg`, then run with `uv run --no-sync`), or numpy 1.26 (TensorFlow 2.20
accepts it; it has no wheels for 3.13).

## 3. The model

The Pi must use the **same files** the server serves (same `model_hash`).
From the server's machine, with `<model-dir>` the model directory in the
server's `config.json`:

```bash
rsync -av <model-dir>/ <user>@<pi>:~/sciot-models/yolo_small_320/
```

The hash must be the same on both machines, and `manifest.json` must have
`input.resize_mode` (a model built before it: `sciot-model build
--overwrite`). On the Pi, in `~/sciot/client`:

```bash
uv run python -c "from sciot_client import segments; print(segments.model_hash('$HOME/sciot-models/yolo_small_320'))"
grep -A6 '"input"' ~/sciot-models/yolo_small_320/manifest.json
```

On the server's machine, at the root of this repository:

```bash
uv run --project client python -c "from sciot_client import segments; print(segments.model_hash('<model-dir>'))"
```

## 4. The server

The Pi must reach it, so it listens on the LAN, not on `127.0.0.1`: the
default `-addr :8080` listens on every interface. `config.json` needs:

- `"record": {"dir": …}`: the analysis needs the recording;
- `"server": {"admin": true}` (or `-admin`): the runs L and W pin the policy.
  The admin endpoint is unauthenticated: use it **only on a trusted network**
  (home or lab LAN, not a public Wi-Fi);
- the model directory of step 3, `offload.algorithm` `latency`,
  `stream.max_inflight` 3.

At the root of this repository (the worker starts with `--project worker`, a
relative path):

```bash
go build -o bin/sciot-server ./cmd/server
bin/sciot-server -config config.json
```

- A built binary, not `go run`: the macOS firewall asks once per binary, and
  `go run` builds a new one every time. Allow incoming connections when asked
  (System Settings → Network → Firewall).
- `<ip>`: on a Mac `ipconfig getifaddr en0` (Wi-Fi) or `en1`.
- From the Pi:

  ```bash
  curl -s http://<ip>:8080/api/v1/server/status
  ```

- Every start writes a new recording, `sciot-<YYYYMMDD-HHMMSS>.jsonl` in
  `record.dir` ([RECORDING.md](RECORDING.md)); the file in use is under
  `record` in the status above. `~/tmp` is in the home directory, not the
  system `/tmp`.

## 5. Clocks

Only the analysis needs them, not the measures: every duration (latency,
device and server times) is measured on one clock, the Pi's or the server's.
But `sciot-replay check` matches the client's CSV (`captured_at`, the Pi's
wall clock) with the server's decisions (the server's wall clock), heartbeat
by heartbeat (1 s): with clocks 2 s apart, frames would be compared with the
wrong decision. They must agree within a few tens of ms.

On the Pi:

```bash
timedatectl status          # "System clock synchronized: yes", "NTP service: active"
timedatectl timesync-status # Offset: a few ms (with chrony instead: chronyc tracking, "System time")
```

On a Mac: System Settings → General → Date & Time → set automatically; the
offset:

```bash
sntp time.apple.com
```

## 6. Configuration and first run

The configuration goes in `~/sciot/pi.yaml`, next to `client/` (anywhere
works: `--config` names it). Relative paths in it are relative to the file,
and `~` is expanded. Every key, with a camera:

```yaml
# ~/sciot/pi.yaml — Raspberry Pi with camera (Picamera2)
server: http://<ip>:8080                  # Go server, LAN address
device_id: pi1                            # unique per device: a second Pi is pi2
model_dir: ~/sciot-models/yolo_small_320  # same files as the server (hash checked)
camera: picamera2                         # Pi camera; the ISP crops and scales
camera_index: 0                           # first camera (CAM0 connector on a Pi 5)
image: ~/sciot/client/sciot_client/stop.png  # used only with --camera static
probe: true                               # answer the server's bandwidth probes
```

Without a camera:

```yaml
# ~/sciot/pi.yaml — Raspberry Pi without camera (static image)
server: http://<ip>:8080
device_id: pi1
model_dir: ~/sciot-models/yolo_small_320
camera: static                            # the image below, prepared once
camera_index: 0                           # ignored with static
image: ~/sciot/client/sciot_client/stop.png  # input image (Det = 1 on yolo-small)
probe: true
```

The command line wins over the file: with a camera, `--camera static` gives
the repeatable runs of step 7.

```bash
cd ~/sciot/client
uv run sciot-client --config ~/sciot/pi.yaml --cycles 30
```

Check:

- with a camera, the line `picamera2: sensor area …, crop …, output …`. The
  area is the sensor mode libcamera chose, read after `configure()` (before
  it, it is `(0, 0, 0, 0)`); with `fit_shortest` the crop is its largest
  centered box with the model's aspect ratio. Seen on a Pi: `sensor area (16,
  0, 2560, 1920), crop (336, 0, 1920, 1920), output (320, 320)`. A small output
  may make libcamera pick a binned or cropped sensor mode: the crop is right
  for that area, the field of view may be narrower than the whole sensor;
- the `Acq ms` and `Dev ms` columns: the first frame's `Acq ms` includes the
  camera start;
- the `Det` column: 1 with the static image; with the camera, at least 1 with
  a stop sign in front of it (a phone screen is fine).

## 7. Phase 3.9 benchmark

Adaptive window (the algorithm chooses split and window) against the fixed
window 3, with the static image for repeatable numbers comparable with
phase 3.8; then the camera, live. Each run ends with Ctrl-C, sent by
`timeout -s INT` (a plain `timeout` would kill the client before it closes
the camera); the 90 s include the TensorFlow start, about 10 s on a Pi.

**Restart the server before every run**: one recording per run. Note the pair
(CSV, recording), e.g. `A1.csv ↔ sciot-20261109-174202.jsonl`.

A pin must come after the registration (a new registration starts unpinned):
start this loop on the server's machine **before** the client; it retries
until the device is registered (`-f`: a 404 is a failure):

```bash
until curl -sf -X POST http://127.0.0.1:8080/api/v1/admin/devices/pi1/policy -d '<policy>'; do sleep 0.5; done; echo
```

The algorithm changes nothing in the first 10 s (`offload.min_hold_ms`), so
the pin is in force from the first frames.

On the Pi, `R` the results directory:

```bash
R=~/sciot/results/phase39; mkdir -p $R
```

| Run | Pi (`cd ~/sciot/client` first) | `<policy>` | Repeat |
|---|---|---|---|
| L: LOCAL | `timeout -s INT 90 uv run sciot-client --config ~/sciot/pi.yaml --camera static --no-table --frames-csv $R/L1.csv` | `{"mode":"LOCAL"}` | 1 |
| A: adaptive | same, `--frames-csv $R/A<n>.csv` | no pin (the algorithm is free) | 3 |
| W: window 3 | same, `--frames-csv $R/W<n>.csv` | `{"mode":"SPLIT","split_point":<S>,"max_inflight":3}`, `<S>` the split chosen in A | 3 |
| C: camera | same without `--camera static`, `--frames-csv $R/C1.csv` | no pin | 1 |

After every run, check the Pi was not throttled (heat or power):

```bash
vcgencmd get_throttled   # throttled=0x0
vcgencmd measure_temp
```

Collect:

- the CSVs, on the server's machine at the root of this repository:
  `rsync -av <user>@<pi>:~/sciot/results/phase39/ client/results/phase39/`;
- the recordings (`record.dir`) with the pairs, the commit of the server and
  `config.json`;
- the Pi model, OS, Python, and the network (Wi-Fi or Ethernet; the same for
  every run). On Wi-Fi keep the Pi in the same place for every run: in another
  room the bandwidth fell from ~75 to ~8 Mbit/s (2026-11-09), which changes
  every SPLIT result. The probes in the recording show it (`probe` events).

The same recordings feed the checks left for devices on other machines: how
often the reservation fallback fires ([OFFLOAD.md](OFFLOAD.md)), the
profiled against the measured server time now that no client shares the
server's CPU ([COSTMODEL.md](COSTMODEL.md)), and `k_noise` / `k_queue` on a
real network.
