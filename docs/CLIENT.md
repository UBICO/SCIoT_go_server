# Python client

`client/` is the reference device client in Python: it runs the model's
segments with TensorFlow Lite, pairs with the server (control plane) and, in
`SPLIT`, sends the activations on the data plane stream. The wire format is
[PROTOCOL.md](PROTOCOL.md); every client follows it, the ESP32 (phase 5)
included.

It is a uv project of its own (`client/pyproject.toml`), separate from the
worker: the device needs neither gRPC nor the worker's code. The two share
no code; the rules both must follow (model hash, decoders) are kept equal by
the files in `testdata/`.

## Install

```bash
uv sync --project client
```

- Python 3.11–3.13. TensorFlow, the runtime the legacy client ran on the
  Raspberry Pi, has no wheels for 3.14 (2.20 and 2.21, the latest).
- TensorFlow 2.20, the worker's version. Its wheels cover macOS (Apple
  silicon), Linux x86-64 and Linux aarch64: on a Raspberry Pi, a 64-bit OS
  (Raspberry Pi OS 64-bit); there are no wheels for 32-bit ARM.

## Modules

```text
control_plane_client.py   v1 protocol: contract types, errors, HTTP calls
        ▲                 (ControlPlaneClient, InferenceWindow, Telemetry)
heartbeat_loop.py         background thread: registration, heartbeat,
        ▲                 policy versioning, LOCAL fallback
http_go_client.py         inference loop: reads hb.policy, runs segments 0..k,
        │                 records the compute time in the window
        ▼
split_pipeline.py         SPLIT: sends frames, handles answers, finishes
        │                 refused frames locally; LOCAL: decodes the output
        ▼
data_plane_client.py      the WebSocket stream: framing, window, receiver thread
segments.py               the model directory: manifest.json, model_hash, input, run(x, first, last)
decoders/                 model output → detections (same as the server's worker)
go_client_config.py       go_client.yaml + command line → server, device, model, image
```

The inference loop never talks to the server. It reads the policy once per
inference and records the compute time in a thread-safe `InferenceWindow`; the
heartbeat thread sends the aggregated window (count, mean, max) once per
interval and applies a newer policy from the answer.

The window also sums every segment run, the prefix of each frame and the
completion of each refused one (`SegmentTimes` forwards them): the heartbeat
sends, per segment of the model, the window's mean and number of runs
(`segment_ms`, `segment_count`; `null` / `0` for a segment not run). They are
raw: the server keeps the moving averages and estimates from them the
device's cost of a split it has not used.

`HeartbeatLoop` keeps the policy accepted from the server separate from the
policy executed:

- server unreachable → execute `LOCAL`, keep the session, retry;
- `409 session_expired` (server restarted) → execute `LOCAL`, register again;
- `404 model_not_found` at registration → LOCAL-only, no retry.

When the server comes back with the same policy version, the device leaves
the fallback: overwriting the accepted policy with `LOCAL` would make it
ignore that version forever.

## Data plane (SPLIT)

In `SPLIT` the device runs segments `0..k` and the server runs `k+1..last`.
The contract (stream, messages, window, deadlines, close codes) is in
[PROTOCOL.md](PROTOCOL.md#data-plane); this section is how this client
follows it, and why.

**One stream, synchronous `websockets`.** The stream is opened with the
current session: right after registering when the server answers
`data_plane: true`, in every mode (in `LOCAL` it carries no frames, but the
server's pings, `policy` messages and probes); otherwise, with an older
server, when the policy becomes `SPLIT`. It uses the synchronous API
of `websockets` (`websockets.sync.client`): plain blocking calls, like
`requests`, so the client keeps its threads (inference loop, heartbeat) and
adds one receiver thread. `send` from the loop and `recv` from the receiver
run at the same time (tested with 200 frames in flight). The asyncio API would
have forced the whole client, TensorFlow included, into an event loop.

**Probes.** The client declares the capability `probe` at registration
(unless `probe: false` in `go_client.yaml` or `--no-probe`) and answers every
`probe_request` from the receiver thread, at once, with a binary `probe` of
the requested size (zeros): the server times its reception, so waiting for
the inference loop would measure the device, not the network. The server asks
only while it has no measurement (no frames for 30 s, e.g. in `LOCAL`).

**Asynchronous frames.** The loop sends frame N and acquires frame N+1 without
waiting. Answers arrive on the receiver thread, which only queues them; the
loop handles them at the start of the next cycle (`SplitPipeline.drain`), so
compute and printing stay on one thread. Results may come back out of order:
they are matched by `sequence_id`.

**The device never waits for the server.** Every frame the server does not
take is finished by the device itself, from the activation it kept (segments
`k+1..last`): the work already done is not lost.

| Situation | What the device does |
|---|---|
| window full (`max_inflight` frames in flight) | runs that frame `LOCAL` at once: the device keeps its pace, a slow server does not stop the acquisition |
| `server_busy`, `deadline_exceeded`, `worker_unavailable` | finishes the frame locally |
| stream closed with frames in flight | finishes each of them locally |
| no stream (refused, unreachable) | finishes frames locally; retries the stream after 1, 2, 4… up to 30 s |
| close `4001` (session expired) | the heartbeat registers again; the stream reopens with the new session |
| close `4002` (another stream with this device id) | stays `LOCAL`: two clients with the same id would close each other in turn |

**Deadline of a frame.** The time this device would need to finish the frame
alone: the mean (EWMA) time of segments `k+1..last` measured on this device,
in `LOCAL` runs and in local completions. A later result is worse than
running `LOCAL`, so the server refuses (`server_busy`) a frame it cannot
serve in time and the device finishes it without waiting. Until those
segments have been measured, `deadline_ms` is not sent and the server's
default applies (1 s). Simplification: the return trip of the result is not
subtracted (below a millisecond on a LAN); phase 3 will revisit it.
`--deadline-ms` forces a fixed value, e.g. to provoke refusals.

**Telemetry.** The heartbeat window keeps measuring the device's compute of
segments `0..k` only; local completions appear in the table and in the final
counters (`frames: {...}`), not in the window.

**`policy` messages.** A policy pushed on the stream goes to
`HeartbeatLoop.offer_policy`, adopted only if its version is newer: it is the
fast path, the heartbeat stays the reliable one.

## Model and decoders

`segments.Model` reads the model directory built by `sciot-model build`
([MODELS.md](MODELS.md)): the segments in execution order and the input from
`manifest.json`, the same file the server reads. The prefix of every frame
(`0..k`) and the completion of a frame the server did not take (`k+1..last`)
run with the same `run(x, first, last)`, timing only the interpreter
invocations. The static image is converted to RGB and resized to the
manifest's input (width and height can differ).

`LOCAL` outputs are decoded on the device with the `postprocess` parameters
received at registration, by `sciot_client/decoders`: the same algorithm as
the worker ([MODELS.md](MODELS.md), "Decoders"), in a separate copy. Both are
tested against the golden files in `testdata/decoders`, and the model hash of
the Go registry, the worker and the client against `testdata/model_hash`: a
copy that diverges fails its tests.

## Running

The device must use the same model files as the server (same `model_hash`):
`model_dir` is the directory built by `sciot-model build` that the server
serves.

The configuration is `client/sciot_client/go_client.yaml`:

| Key | Default | Meaning |
|---|---|---|
| `server` | `http://127.0.0.1:8080` | Go server URL |
| `device_id` | — (required) | unique per device: the server keeps one session per id |
| `model_dir` | — (required) | directory built by `sciot-model build` |
| `image` | `stop.png` | static input image, resized to the model input |
| `probe` | `true` | declare the `probe` capability: answer the server's bandwidth probes (~512 KB every 30 s while the network is not measured); `false` withdraws it, e.g. on a metered link |

- Command-line options win over the file: `--server`, `--device-id`,
  `--model-dir`, `--image`, `--no-probe`. `--config PATH` (or `$SCIOT_GO_CLIENT_CONFIG`)
  reads another file; the default file is optional.
- Relative paths are relative to the file (on the command line, to the
  working directory). Unknown keys are an error.

```bash
uv run --project client sciot-client --model-dir ~/sciot-models/yolo_small_320
```

Other options: `--cycles N` (stop after N inferences), `--no-table` (no
per-frame table), `--frames-csv PATH`, `--deadline-ms` (fixed SPLIT deadline).

The benchmarks write their own config file (every key explicit, the
client's `stop.png`) and pass `--config`: a locally edited `go_client.yaml` does not
change their results.

One row per frame, printed when its outcome is known:

```text
    Seq | Mode  | Split | Acq ms | Dev ms | Srv ms | Recv ms | Net ms | Det | Outcome
     12 | SPLIT |     4 |    0.0 |   10.1 |   34.3 |     0.4 |    0.8 |   1 | result
      — | SPLIT |     4 |    0.0 |   64.9 |      — |       — |      — |   1 | local (window_full)
      — | LOCAL |     — |    0.0 |   65.2 |      — |       — |      — |   1 | local
```

`Dev ms` is the device's compute (prefix, plus the completion when finished
locally); `Recv ms` is the server's reading of the frame, first to last byte
(`timing.receive_ms`): the upload without the latency, possibly
underestimated when TCP had already buffered the bytes; `Net ms` is the round
trip minus the server time. The frames CSV has the same columns
(`recv_ms`).

Pairing check without running inferences:

```bash
uv run --project client python -m sciot_client.control_plane_client --device-id web_96 --model-dir ~/sciot-models/yolo_small_320
```

## Benchmarks

The benchmarks build the server from this repository, start it with its
worker, and run the client; results go to `client/results/` (git-ignored,
or `$SCIOT_RESULTS_DIR`). `--go-bin` uses a prebuilt server instead.

The comparison with the legacy client (`bench_control_plane.py`, 2026-10-10:
64.5 against 76.4 ms per cycle in `LOCAL`, 35 against 255 768 bytes sent per
cycle) stays in SCIoT_python_client, with the legacy server it needs.

## SPLIT benchmark

`scripts/bench_split.py` measures every configuration of one model: `LOCAL`
and every split point `-1..last-1` (yolo-small: 9 configurations).

```bash
uv run --project client python client/scripts/bench_split.py --model-dir ~/sciot-models/yolo_small_320
```

**Method.** One Go server (worker, `-admin`, recording) and one client that
never restarts: the benchmark changes the policy from the admin endpoint, as
phase 3's algorithm will. For each configuration, `--runs` times (default 3)
in alternating order: set the policy, wait for the first frame computed with
its version, wait `--warmup` seconds (default 5), measure for `--measure`
seconds (default 20). Frames are attributed to a configuration by their
`policy_version`, never by time, so transitions cannot mix them. About
10 minutes with the defaults; `--configs LOCAL,-1,4` measures a subset.

**Sources.** The client's `--frames-csv` (what the device sees, LOCAL frames
included), the server's recording (queue waits, prediction errors) and a
TCP proxy counting the bytes.

| Column | Meaning |
|---|---|
| Frames/s | frames completed per second, by the server or by the device |
| Latency | acquisition → outcome. For a server result, at its **arrival** (acquisition + device compute + round trip), independently of when the client loop reads it; for a frame finished locally, when the device is done with it, waits included |
| Device | device compute of the frames the server answered (the offloaded prefix); for `LOCAL`, of every frame. Frames finished locally are counted by "Server results", not mixed in here: their two populations would make the median jump |
| Server, Network | median server time (queue included) and round trip minus server time, of the server's results |
| Sent KiB/frame | payload sent per completed frame (0 for frames never sent) |
| Server results | share of frames the server answered; the rest were finished locally (window full, `server_busy`, `deadline_exceeded`) |
| Detections ≠ LOCAL | frames whose number of detections differs from `LOCAL`: SPLIT must give the same answer |

`results.json` also has, per phase, every outcome, the queue wait and the
dispatcher's prediction error (measured wait − predicted wait).

Reference result (yolo-small 320×320, the client's static image, localhost,
MacBook with 12 cores, worker 2 slots × 2 threads, window 3, defaults:
3 runs × 20 s per configuration, 2026-10-21):

| Config | Frames/s | Latency p50 (ms) | Latency p95 (ms) | Device (ms) | Server (ms) | Network (ms) | Sent KiB/frame | Server results | Detections ≠ LOCAL |
|---|---|---|---|---|---|---|---|---|---|
| LOCAL | 14.8 ± 0.1 | 65.4 ± 0.2 | 68.8 ± 0.3 | 65.0 ± 0.1 | — | — | 0 ± 0 | 0 ± 0 % | 0 |
| split -1 | 29.0 ± 0.0 | 58.8 ± 0.7 | 137.0 ± 0.3 | 0.0 ± 0.0 | 37.7 ± 0.0 | 1.4 ± 0.1 | 899 ± 1 | 50 ± 0 % | 0 |
| split 0 | 27.9 ± 0.3 | 63.3 ± 3.1 | 138.8 ± 2.0 | 0.5 ± 0.0 | 39.2 ± 0.2 | 2.9 ± 0.1 | 2400 ± 0 | 50 ± 0 % | 0 |
| split 1 | 46.3 ± 0.9 | 59.2 ± 5.9 | 69.3 ± 1.1 | 3.2 ± 0.0 | 36.0 ± 0.2 | 1.5 ± 0.0 | 1215 ± 6 | 74 ± 0 % | 0 |
| split 2 | 46.3 ± 0.4 | 55.1 ± 1.4 | 68.1 ± 0.2 | 3.7 ± 0.0 | 35.6 ± 0.3 | 1.5 ± 0.1 | 1205 ± 5 | 75 ± 0 % | 0 |
| split 3 | 40.6 ± 0.3 | 54.6 ± 0.2 | 67.9 ± 0.5 | 7.5 ± 0.0 | 33.4 ± 0.3 | 1.6 ± 0.1 | 1200 ± 0 | 75 ± 0 % | 0 |
| split 4 | 46.3 ± 0.7 | 49.0 ± 0.3 | 67.3 ± 0.4 | 10.2 ± 0.0 | 37.4 ± 0.2 | 1.1 ± 0.1 | 667 ± 0 | 83 ± 0 % | 0 |
| split 5 | 49.4 ± 0.6 | 48.8 ± 0.6 | 67.4 ± 0.5 | 10.8 ± 0.0 | 36.1 ± 0.3 | 1.1 ± 0.1 | 694 ± 1 | 87 ± 0 % | 0 |
| split 6 | 49.9 ± 0.2 | 45.8 ± 0.2 | 47.5 ± 0.8 | 17.6 ± 0.0 | 27.1 ± 0.2 | 1.1 ± 0.1 | 800 ± 0 | 100 ± 0 % | 0 |

- Split 6 is the best on both counts: 3.4× the frames of `LOCAL` with a lower
  latency, and the server answers every frame.
- Splits -1 and 0 are bounded by the server: with 2 slots and S ≈ 37 ms it
  serves about 54 frames/s, fewer than the device produces, so half of the
  frames are finished locally. This depends on the worker's size (2 × 2 uses
  4 of 12 cores here), not on the split.
- Split 0 sends 2.4 MB per frame, twice the model input: the worst choice on
  a real network.
- The dispatcher's predicted queue wait is off by less than 1 ms (p95) in
  every configuration; detections are identical to `LOCAL` in every frame.

**Limits.** A static image (acquisition ~0 ms), localhost (network almost
free), one device. With a mix of server results and local completions (e.g.
split `-1`) the latency has two populations and its median depends on which
one is larger: read it together with "Server results". Real networks:
`--go-target` is not supported yet (the benchmark starts its own server);
network conditions and many devices are phase 4's scenarios.

## Network benchmark

`scripts/bench_network.py` checks the server's network estimate
([RECORDING.md](RECORDING.md#network)) against a known bandwidth: the client, in
SPLIT at `--split` (default `-1`, the model input), talks to the server
through a proxy that caps the upload like one link.

```bash
uv run --project client python client/scripts/bench_network.py --model-dir ~/sciot-models/yolo_small_320 --limits-mbps 10,50,200,0
```

`--split LOCAL` sends no frames: the server measures with probes. Pass
`--probe-interval-ms` (e.g. 2000) to get several probes per limit instead of
one every 30 s.

For every limit (0 = none): set it, wait `--warmup` seconds (default 15: the
estimate is a moving average), measure `--measure` seconds (default 20). From
the server's recording: the estimate (median over the heartbeats of the
window), the per-frame bandwidth of the valid samples, valid/discarded
samples, "not limiting", latency, `receive_ms`; from the proxy, the
throughput really carried (the client may not fill the link).

Reference results (yolo-small 320×320, split -1: 1.2 MB per frame,
localhost, MacBook, worker 2 × 2, 2026-10-23). Limits `10,50,200,0`:

| Limit (Mbit/s) | Carried (Mbit/s) | Estimate (Mbit/s) | Estimate / limit | Valid frames (Mbit/s) | Valid / discarded | Not limiting | Latency (ms) | Receive p50 (ms) |
|---|---|---|---|---|---|---|---|---|
| 10.0 | 10.0 | 10.0 | 1.00 | 10.0 | 19 / 0 | False | 983.16 | 983.4 |
| 50.0 | 50.0 | 50.0 | 1.00 | 50.0 | 97 / 0 | False | 156.10 | 196.6 |
| 200.0 | 199.0 | 202.6 | 1.01 | 200.1 | 386 / 0 | False | 1.87 | 49.1 |
| none | 212.3 | 204.8 | — | — | 0 / 414 | True | 0.10 | 0.6 |

Limits `0,10,50` (the link free first):

| Limit (Mbit/s) | Carried (Mbit/s) | Estimate (Mbit/s) | Estimate / limit | Valid frames (Mbit/s) | Valid / discarded | Not limiting | Latency (ms) | Receive p50 (ms) |
|---|---|---|---|---|---|---|---|---|
| none | 213.8 | — | — | — | 0 / 414 | True | 0.10 | 0.6 |
| 10.0 | 10.0 | 10.0 | 1.00 | 10.0 | 19 / 0 | False | 0.10 | 983.4 |
| 50.0 | 49.9 | 50.0 | 1.00 | 50.0 | 97 / 0 | False | 74.19 | 196.6 |

- **Bandwidth**: within 1% of the limit whenever the link is the
  bottleneck, every sample valid. Without a limit every sample is discarded
  and the network is "not limiting" (the 204.8 is the average left from the
  previous phase: ignore it when `not_limiting`); the 212 Mbit/s carried are
  bounded by the worker, not the link.
- **Latency**: with the link saturated a pong waits behind the bytes in the
  client's TCP send buffer (~1 MB here), so a ping measures the queue: 983 ms
  at 10 Mbit/s, 156 ms at 50. The server keeps the minimum of the last 60 s,
  so a value measured while the link was free survives: 0.10 ms at
  10 Mbit/s when the free phase came first. After more than 60 s of
  continuous saturation (50 Mbit/s, second table) the queue comes back.
  In the first table the stream was never free: it opens in SPLIT and the
  limit is set at the first frame.
- `receive_ms` is bytes / bandwidth (1.2 MB at 10 Mbit/s = 983 ms).

## Tests

```bash
cd client && uv run pytest
```

The `external_server` tests are skipped by default; they need a running
server (the policy-change test also `-admin`) and its model directory:

```bash
cd client && SCIOT_GO_SERVER_URL=http://127.0.0.1:8080 SCIOT_MODEL_DIR=~/sciot-models/yolo_small_320 uv run pytest -m external_server
```
