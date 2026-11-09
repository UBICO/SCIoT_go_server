# Raspberry Pi device

A step-by-step guide: the Python client ([CLIENT.md](CLIENT.md)) on a
Raspberry Pi with its camera, against a Go server on another machine (here a
Mac) over a real network, then the phase 3.9 benchmark.

Lines marked **check** are facts not yet verified on a Pi: note what you see.

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

- **Camera** connected and seen by libcamera:

  ```bash
  rpicam-hello --list-cameras   # libcamera-hello on older images
  ```

## 2. Install

Picamera2 is a Raspberry Pi OS package, not on PyPI: the client's venv is
built on the system Python with `--system-site-packages`, so it sees it.

```bash
sudo apt update
sudo apt install -y python3-picamera2 rsync
curl -LsSf https://astral.sh/uv/install.sh | sh   # then open a new shell
```

The client from the Mac (or `git clone` of this repository on the branch you
want):

```bash
rsync -av --exclude .venv --exclude results ~/Documents/Ricerca/sciot-go-server/client/ pi@<pi>:~/sciot/client/
```

On the Pi:

```bash
cd ~/sciot/client
uv venv --system-site-packages --python /usr/bin/python3
uv sync --frozen
```

`uv sync` keeps the existing venv: `--python /usr/bin/python3` is the
interpreter Picamera2 was built for. No `--extra opencv`: the Pi does not use
OpenCV.

**Check numpy.** The venv's numpy (from PyPI, 2.x) hides the apt one, and
Picamera2's compiled dependencies (e.g. `simplejpeg`) were built against the
apt numpy. **check** that both import:

```bash
uv run python -c "import numpy, picamera2, tensorflow as tf; print(numpy.__version__, tf.__version__)"
```

An error like `numpy.dtype size changed` or `module compiled against ABI
version` means a mismatch: send it. Possible fixes, to try in this order:
the PyPI build of the failing module in the venv (`uv pip install simplejpeg`,
then run with `uv run --no-sync`), or numpy 1.26 (TensorFlow 2.20 accepts it;
it has no wheels for 3.13).

## 3. The model

The Pi must use the **same files** the server serves (same `model_hash`).
From the Mac:

```bash
rsync -av ~/Documents/Ricerca/sciot-models/yolo_small_320/ pi@<pi>:~/sciot-models/yolo_small_320/
```

On the Pi, the hash must be the server's (`b1823f4505d912e7b390f450f39832f8`
for yolo-small built on 2026-11-07); its `manifest.json` must have
`input.resize_mode`:

```bash
uv run python -c "from sciot_client import segments; print(segments.model_hash('$HOME/sciot-models/yolo_small_320'))"
```

## 4. The server on the Mac

The Pi must reach it, so it listens on the LAN, not on `127.0.0.1`. The admin
endpoint is unauthenticated: use it **only on a trusted network** (home or
lab LAN, not a public Wi-Fi).

```bash
go build -o bin/sciot-server ./cmd/server
bin/sciot-server -config config.json -addr :8080 -admin
```

- A built binary, not `go run`: the macOS firewall asks once per binary, and
  `go run` builds a new one every time. Allow incoming connections when asked
  (System Settings → Network → Firewall).
- The Mac's address: `ipconfig getifaddr en0` (Wi-Fi) or `en1`.
- From the Pi:

  ```bash
  curl -s http://<mac>:8080/api/v1/server/status
  ```

- `config.json`: the same model directory, `offload.algorithm` `latency`,
  `record.dir` set (the analysis needs the recording),
  `stream.max_inflight` 3.

## 5. Clocks

`sciot-replay check` aligns the client's `captured_at` with the server's
recording, so the two clocks must agree within a few tens of ms (the
heartbeat is 1 s).

```bash
timedatectl status   # on the Pi: "System clock synchronized: yes"
```

On the Mac: System Settings → General → Date & Time → set automatically.

## 6. Configuration and first run

`~/sciot/pi.yaml` on the Pi (relative paths are relative to this file):

```yaml
server: http://<mac>:8080
device_id: pi1
model_dir: ~/sciot-models/yolo_small_320
camera: picamera2
camera_index: 0
image: client/sciot_client/stop.png
probe: true
```

```bash
cd ~/sciot/client
uv run sciot-client --config ~/sciot/pi.yaml --cycles 50
```

Send:

- the line `picamera2: sensor area …, crop …, output …; area after configure …`.
  **check**: the two areas are equal; if the second is smaller, libcamera
  chose a cropped sensor mode and the crop is wrong;
- the `Acq ms` and `Dev ms` columns of a few rows;
- with a stop sign in front of the camera (a phone screen is fine): the `Det`
  column, at least 1.

## 7. Phase 3.9 benchmark

Adaptive window (the algorithm chooses split and window) against the fixed
window 3, with the static image for repeatable numbers comparable with
phase 3.8; then the camera, live. Each run ends with Ctrl-C, sent by
`timeout -s INT` (a plain `timeout` would kill the client before it closes
the camera).

On the Pi, `R` the results directory:

```bash
R=~/sciot/results/phase39; mkdir -p $R
```

| Run | Pi | Mac (admin) | Repeat |
|---|---|---|---|
| L: LOCAL | `timeout -s INT 90 uv run sciot-client --config ~/sciot/pi.yaml --camera static --no-table --frames-csv $R/L1.csv` | as soon as it registers: `curl -X POST http://127.0.0.1:8080/api/v1/admin/devices/pi1/policy -d '{"mode":"LOCAL"}'` | 1 |
| A: adaptive | same, `--frames-csv $R/A<n>.csv` | nothing (the algorithm is free) | 3 |
| W: window 3 | same, `--frames-csv $R/W<n>.csv` | `-d '{"mode":"SPLIT","split_point":<S>,"max_inflight":3}'`, `<S>` the split chosen in A | 3 |
| C: camera | same without `--camera static`, `--frames-csv $R/C1.csv` | nothing | 1 |

Between runs, check the Pi was not throttled (heat or power):

```bash
vcgencmd get_throttled   # throttled=0x0
vcgencmd measure_temp
```

Send:

- the CSVs (`rsync -av pi@<pi>:~/sciot/results/phase39/ client/results/phase39/`
  on the Mac);
- the server's recording directory (`record.dir`), the commit of the server
  and `config.json`;
- the Pi model, OS, Python, and the network (Wi-Fi or Ethernet; the same for
  every run).

The same recordings feed the checks left for devices on other machines: how
often the reservation fallback fires ([OFFLOAD.md](OFFLOAD.md)), the
profiled against the measured server time now that no client shares the
server's CPU ([COSTMODEL.md](COSTMODEL.md)), and `k_noise` / `k_queue` on a
real network.
