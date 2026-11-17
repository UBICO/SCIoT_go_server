# Raspberry Pi device

A step-by-step guide: the Python client ([CLIENT.md](CLIENT.md)) on a
Raspberry Pi, with or without its camera, against a Go server on another
machine over a real network, then the phase 3.9 benchmark and the phase 4.2
campaign.

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

The checks left for devices on other machines (reservation fallback,
profiled against measured server time, `k_noise` / `k_queue` on a real
network) have their own campaign, step 8.

## 8. Phase 4.2 campaign

One campaign for the phase 4.2 checks: the client's `answer_ms` and the
window chosen for frames per second (`p95_slack`) on a real device, then two
devices on other machines than the server (reservation fallback, CPU
contention, `k_noise` / `k_queue`, oscillations), two different models,
and a slow network. Same rules as step 7: server restarted before every run,
one recording per run, 90 s, `timeout -s INT`, `vcgencmd get_throttled`
after each.

**Before the runs:**

- Server and both clients from the **same commit**, `47e760a` or later (the
  server rejects `answer_ms` before phase 4.2): `rsync` `client/` to both Pis
  (step 2) and `uv sync --frozen` there. Note the commit.
- `config.json` as in step 4 (`latency`, `stream.max_inflight` 3, admin,
  `record.dir`), `offload.p95_slack` left at its default (0.25). Two copies
  for R4, differing only in the `offload` section:
  `config-knoise0.json` (`"k_noise": 0`) and `config-kqueue0.json`
  (`"k_queue": 0`; the key was removed in phase 4.3 and a config that
  still has it no longer starts, so R4 now runs `k_noise` only).
- R5 needs a second model, different from yolo-small: export and build it
  ([MODELS.md](MODELS.md)), add it to `models` in all three `config.json`,
  and copy its directory to `pi2` (`~/sciot-models/<model_key>`).
- `pi2` gets its own `~/sciot/pi.yaml` with `device_id: pi2` (step 6). Both
  Pis in the same room as in step 7 (~75 Mbit/s), with the static image,
  except R6.

On each Pi: `mkdir -p ~/sciot/results/phase42`. A run with one device is
the client command of step 7 with `--frames-csv
~/sciot/results/phase42/<CSV>`.

**Two devices together** (R3–R5): start both from the server's machine, so
that they register within a second:

```bash
ssh <user>@<pi1> "cd ~/sciot/client && timeout -s INT 90 uv run sciot-client --config ~/sciot/pi.yaml --camera static --no-table --frames-csv ~/sciot/results/phase42/<run>_pi1.csv" &
ssh <user>@<pi2> "cd ~/sciot/client && timeout -s INT 90 uv run sciot-client --config ~/sciot/pi.yaml --camera static --no-table --frames-csv ~/sciot/results/phase42/<run>_pi2.csv" &
wait
```

Pins as in step 7 (the loop on the server's machine before the client,
`/admin/devices/<device_id>/policy`).

| Run | Devices | Server config | `<policy>` | Repeat | CSV | Feeds |
|---|---|---|---|---|---|---|
| R1: free | pi1 | `config.json` | no pin | 3 | `R1_<n>.csv` | `answer_ms` and the cost model; expected split −1, window 2 (reason `fps`) |
| R2: fixed windows | pi1 | `config.json` | `{"mode":"SPLIT","split_point":-1,"max_inflight":<w>}`, `<w>` 1, 2, 3 | 1 each | `R2_w<w>.csv` | the frames/s–latency curve; predicted against measured per window |
| R3: two devices | pi1 + pi2 together | `config.json` | no pin | 3 | `R3_<n>_<pi>.csv` | reservation fallback, `S` profiled vs measured, two real devices as clients, policy changes |
| R4: ablation | pi1 + pi2 together | `config-knoise0.json`, then `config-kqueue0.json` | no pin | 2 + 2 | `R4_knoise0_<n>_<pi>.csv`, `R4_kqueue0_<n>_<pi>.csv` | `k_noise` / `k_queue` on a real network |
| R5: two models | pi1 (yolo-small) + pi2 (second model, `--model-dir ~/sciot-models/<model_key>`) together | `config.json` | no pin | 2 | `R5_<n>_<pi>.csv` | heterogeneous devices (different models and split points) |
| R6: slow network | pi2 in the other room (~8 Mbit/s, step 7) | `config.json` | no pin | 2 | `R6_<n>.csv` | slow link: upload, Wi-Fi stalls, window choice |

About 17 runs, ~45 minutes with the restarts. Collect as in step 7:

```bash
rsync -av <user>@<pi1>:~/sciot/results/phase42/ client/results/phase42/
rsync -av <user>@<pi2>:~/sciot/results/phase42/ client/results/phase42/
```

and write `client/results/phase42/README.md` (gitignored, as for phase 3.9):
commit, `config.json` of each run, for every run the pairs CSV ↔ recording,
temperatures, and anything unusual (a Pi moved, a run repeated).

**Run** (2026-11-10 and 2026-11-17, `client/results/phase42/`): R1 ×4 (R1_4
after the window hysteresis fix), R2, R3 ×3, R4 2 + 2, R5 ×2 with yolo-pico
320 on `pi2`; R6 not run. No throttling, 44–58 °C. Results:
[COSTMODEL.md](COSTMODEL.md#verification-on-the-pi-with-answer_ms-phase-42-campaign)
(cost model, CPU contention, two devices, two models) and
[OFFLOAD.md](OFFLOAD.md) (window, other devices, `k_noise` / `k_queue`).
The slow network (R6) is still to run; the loaded server ran in
[step 9](#9-phase-43-campaign).

## 9. Phase 4.3 campaign

Two open checks of phase 4: a **loaded server** (utilization above 0.8,
never reached with two Pis on 2 slots) and **yolo-pico** after the
bandwidth from the time per byte. Same rules as step 8 (server restarted
before every run, both Pis started together from the server's machine,
90 s, static image, `vcgencmd get_throttled` after each); CSVs in
`~/sciot/results/phase43/` on the Pis.

**Before the runs:**

- Server and both clients from the same commit (`rsync` `client/`, `uv sync
  --frozen`). Over a non-interactive `ssh` the Pi's shell does not read the
  `PATH` the uv installer added: call `~/.local/bin/uv`, or prefix the
  command with `export PATH=$HOME/.local/bin:$PATH;`.
- The load comes from a worker with **1 slot** instead of more devices:
  copies of `config.json` differing only in `worker`: `"slots": 1` with
  `"threads": 2` (`S` at split −1 ~36 ms on a MacBook: two Pis at window 2
  give ρ ~0.75) and with `"threads": 1` (`S` ~67 ms: ρ ≥ 1 offered). The
  dispatcher, the simulation and the worker take any number of slots ≥ 1.

| Run | Devices | Worker | `<policy>` | Repeat |
|---|---|---|---|---|
| Q1: free | pi1 + pi2, yolo-small | 1 slot × 2 threads | no pin | 2 |
| Q2: free, `k_queue: 0` | same | same | no pin | 2 |
| Q3: window 3 | same | same | both `{"mode":"SPLIT","split_point":-1,"max_inflight":3}` | 1 |
| Q4: saturated | same | 1 slot × 1 thread | both split −1, `max_inflight` 2 | 1 |
| Q5: free, saturated | same | 1 slot × 1 thread | no pin | 1 + 1 with `k_queue: 0` |
| P1: pico free | pi1 yolo-small, pi2 yolo-pico (`--model-dir`) | `config.json` (2 slots) | no pin | 2 |
| P2: pico windows | same | same | pi2 `{"mode":"SPLIT","split_point":4,"max_inflight":<w>}`, `<w>` 1, 2, 3 | 1 each |

`k_queue` was removed after this campaign ([OFFLOAD.md](OFFLOAD.md#verification-phase-38)):
a config with it no longer starts.

**Run** (2026-11-17, commit `59dd88d`, `client/results/phase43/`): the 13
runs, P1/P2 repeated once (the first attempt pointed `--model-dir` at a
path that does not exist on the Pi, so `pi2` never started). No
throttling, 43–65 °C. Results:
[COSTMODEL.md](COSTMODEL.md#loaded-server-and-pico-on-the-pi-phase-43-campaign)
(simulation near saturation, queue formulas, pico) and
[OFFLOAD.md](OFFLOAD.md#verification-phase-38) (`k_queue`, window). Still
to run: the slow network (R6 of step 8).
