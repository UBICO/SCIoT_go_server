# SCIoT_go_server

Go server for SCIoT distributed inference. Edge devices run a TFLite model
split into segments; for every device the server decides whether it runs the
model fully on the device (`LOCAL`) or only its first segments, sending the
activation to the server, which runs the rest (`SPLIT`).

- **Control plane** (HTTP + JSON): pairing, model information, a heartbeat
  with the device's telemetry (1 Hz by default) and the versioned execution
  policy. The inference loop on the device never waits for it: it reads the
  current policy locally.
- **Data plane** (one WebSocket per device): the activations of `SPLIT`
  frames and their results, already decoded into detections. Frames are
  asynchronous; a frame the server cannot serve within its deadline is
  refused and the device finishes it itself.

The server is written in Go; the TFLite segments run in a Python worker
(`worker/`) that the server starts and supervises, on the same machine, over
gRPC (why: [docs/spikes/ML_RUNTIME.md](docs/spikes/ML_RUNTIME.md)).

Status:

- [x] phase 1, control plane: registration, model information, heartbeat with
  windowed telemetry, versioned `LOCAL`/`SPLIT` policy, session expiry,
  opt-in admin endpoint to force a policy
- [x] phase 2, data plane: stream, admission by deadline with a FIFO queue,
  worker supervisor, decoders, model build from Edge Impulse exports,
  recording of events for offline evaluation
- [ ] phase 3, offloading algorithm: cost model with a simulation of device
  and queue, `latency` algorithm (opt-in, `offload.algorithm`; by default
  the policy changes only from the admin endpoint), verification in progress

The reference device client is in `client/`, in Python
([docs/CLIENT.md](docs/CLIENT.md), with the SPLIT and network benchmarks).

## Documentation

| Document | What |
|---|---|
| [docs/PROTOCOL.md](docs/PROTOCOL.md) | the v1 protocol: control plane and data plane, the contract for every client |
| [docs/MODELS.md](docs/MODELS.md) | from Edge Impulse to a model directory (`sciot-model build`), manifest, model hash, decoders and golden files |
| [docs/RECORDING.md](docs/RECORDING.md) | the JSON Lines recording (`sciot-record.v1`) and how to read it |
| [docs/OFFLOAD.md](docs/OFFLOAD.md) | the offloading algorithm: constraints, objective, hysteresis, admin pin, configuration |
| [docs/COSTMODEL.md](docs/COSTMODEL.md) | predicted cost of a frame per split point: the simulation of device and queue, and its verification (`sciot-replay algo`, `sciot-replay queue`) |
| [docs/spikes/ML_RUNTIME.md](docs/spikes/ML_RUNTIME.md) | why a Python worker over gRPC, with measurements |
| [worker/README.md](worker/README.md) | the Python worker and the model build tool |
| [docs/CLIENT.md](docs/CLIENT.md) | the Python device client (`client/`): install, configuration, benchmarks |

## Requirements

- Go 1.27 or newer (one external dependency: gopsutil, for the load metrics);
- [uv](https://docs.astral.sh/uv/) and Python 3.11–3.13 for the worker and
  the client (TensorFlow 2.20, installed by uv on first run).

## Quick start

1. **Build the model directory** from the two Edge Impulse exports
   (details in [docs/MODELS.md](docs/MODELS.md)). The server and the devices
   must use the same directory: a rebuild changes the model hash.

   ```bash
   uv run --project worker sciot-model build <savedmodel.zip> --ei-cpp-export <cpp.zip> --sample image.png --output ~/sciot-models/yolo_small_320
   ```

2. **Configure** `config.json` (next section) with the model directory and a
   `worker` section, then **start the server**. It starts the worker and
   enables the data plane when the log says `worker ready`.

   ```bash
   go run ./cmd/server -config config.json -addr 127.0.0.1:8080 -admin
   ```

3. **Start a device**: the Python client (`client/`), with the same model
   directory.

   ```bash
   uv run --project client sciot-client --server http://127.0.0.1:8080 --model-dir ~/sciot-models/yolo_small_320
   ```

## Configuration

Copy `config.example.json` to `config.json` (git-ignored, it contains local
paths):

```json
{
  "server": {
    "heartbeat_interval_ms": 1000,
    "verbose": false,
    "admin": false,
    "load_sample_interval_ms": 1000,
    "load_cpu_ewma_alpha": 0.35
  },
  "worker": {
    "command": [
      "uv",
      "run",
      "--project",
      "worker",
      "sciot-worker"
    ],
    "address": "127.0.0.1:50051",
    "slots": 2,
    "threads": 6,
    "startup_timeout_ms": 120000
  },
  "stream": {
    "max_inflight": 3,
    "default_deadline_ms": 1000,
    "max_message_bytes": 8388608
  },
  "models": [
    {
      "model_key": "yolo_small_320",
      "tflite_dir": "/path/to/sciot-models/yolo_small_320"
    }
  ]
}
```

| Key | Default | Meaning |
|---|---|---|
| `server.heartbeat_interval_ms` | 1000 | interval sent to clients, 100–60000 |
| `server.verbose` | false | one table row per accepted heartbeat |
| `server.admin` | false | enable the unauthenticated admin endpoint |
| `server.load_sample_interval_ms` | 1000 | how often the server samples its own CPU/memory load, 100–60000 |
| `server.load_cpu_ewma_alpha` | 0.35 | weight of the newest CPU sample in the moving average, (0, 1] |
| `worker` | absent | data plane on/off: without this section `/api/v1/stream` does not exist and every device stays LOCAL |
| `worker.command` | [] | command that starts the Python worker (the server adds `--listen`, `--slots`, `--threads`, `--model`); empty = external worker, started by someone else |
| `worker.address` | — | worker gRPC address, `host:port` on the same machine |
| `worker.slots`, `worker.threads` | — | parallel inferences × TFLite threads each; required when `command` is set |
| `worker.startup_timeout_ms` | 120000 | time for the worker to load TensorFlow and the models before a restart |
| `stream.max_inflight` | 3 | frames per device in flight at the same time |
| `stream.default_deadline_ms` | 1000 | frame deadline when the client sends none |
| `stream.max_message_bytes` | 8388608 | largest WebSocket message, 1024–67108864 |
| `stream.ping_interval_ms` | 1000 | ping on the stream to measure each device's latency, 100–60000; 0 = never ([docs/RECORDING.md](docs/RECORDING.md#network)) |
| `stream.probe_interval_ms` | 30000 | ask a probe of a device (with the `probe` capability) whose network has not been measured for this long, 1000–3600000; 0 = never ([docs/PROTOCOL.md](docs/PROTOCOL.md#probe)) |
| `stream.probe_unknown_interval_ms` | 5000 | the same, while the device's bandwidth has no estimate (a link where reads never wait needs 3 probes in 60 s to be known as not limiting), 1000–3600000; 0 = always `probe_interval_ms` |
| `stream.probe_bytes` | 524288 | size of a probe, 65536 to `max_message_bytes` − 64 (by default capped to fit `max_message_bytes`) |
| `stream.bandwidth_tau_ms` | 60000 | time constant of the bandwidth estimate, 100–3600000 ([docs/RECORDING.md](docs/RECORDING.md#estimates)) |
| `dispatch.service_tau_ms` | 10000 | time constant of the service time estimates `S` (per split point, segment, overhead), 100–3600000 |
| `dispatch.profile_runs` | 5 | when the worker becomes ready, run each whole model this many times (plus one warm-up) on a zero input, so `S` is known for every split point before any device uses it, 0–100; 0 = no profile ([docs/RECORDING.md](docs/RECORDING.md#profile--the-server-profiled-a-model)) |
| `dispatch.reprofile_age_ms` | 30000 | profile a model again when the estimate of one of its segments is older than this (segments no frame runs), 100–3600000; 0 = never ([docs/OFFLOAD.md](docs/OFFLOAD.md#reactivity)) |
| `offload.*` | `static` | the offloading algorithm and its parameters ([docs/OFFLOAD.md](docs/OFFLOAD.md#configuration)) |
| `server.device_segment_tau_ms` | 10000 | time constant of the estimates of the device's time per segment, 100–3600000 |
| `record.dir` | absent | write every event (sessions, heartbeats, policy changes, SPLIT frames) to a JSON Lines file in this directory, one file per start ([docs/RECORDING.md](docs/RECORDING.md)) |
| `models[].tflite_dir` | — | model directory built by `sciot-model build` ([docs/MODELS.md](docs/MODELS.md)): segments and `manifest.json` |
| `models[].postprocess` | — | optional override of `score_threshold`, `iou_threshold`, `max_detections` |

Unknown keys are rejected. Segment count, input and post-processing come
from the model's `manifest.json`; the server refuses to start if a manifest
is missing or does not match the files in its directory, or if two models
have the same segments.

## Running

```bash
go run ./cmd/server -config config.json -addr :8080
```

Flags `-verbose` and `-admin` switch the corresponding options on (they
cannot switch them off: use `config.json` for that). With `-admin`, bind to
localhost: `-addr 127.0.0.1:8080`.

Force a policy on a registered device (admin enabled). In `SPLIT`,
`split_point` is the last segment the device runs, from `-1` (everything on
the server) to the second-to-last segment:

```bash
curl -X POST http://127.0.0.1:8080/api/v1/admin/devices/web_96/policy -d '{"mode":"SPLIT","split_point":5}'
```

A forced policy pins the device: with `offload.algorithm` `latency` the
algorithm leaves it alone until it is released:

```bash
curl -X DELETE http://127.0.0.1:8080/api/v1/admin/devices/web_96/policy
```

State of the server (load, dispatcher queue: slots, λ, ρ, service time per
split, recording counters):

```bash
curl -s http://127.0.0.1:8080/api/v1/server/status
```

On `SIGINT`/`SIGTERM` the server closes the streams (`1001`), stops the
worker and closes the recording.

## Development

```bash
gofmt -l . && go vet ./... && go test -race ./...
```

```bash
cd worker && uv run pytest
```

```bash
cd client && uv run pytest
```

The worker and client tests build tiny TFLite models at run time and check
the decoders and the model hash against the files in `testdata/`: no model
artifacts needed.

Layout:

```text
cmd/server/          main: flags, wiring, http.Server with timeouts
internal/api/        protocol types and their validation (no dependencies)
internal/config/     config.json → registry.Config, server.Config
internal/registry/   model_hash → model info, computed at startup
internal/device/     in-memory device state (sessions, policy, telemetry)
internal/load/       server host load, sampled in its own goroutine (gopsutil)
internal/server/     HTTP handlers, verbose heartbeat table
internal/stream/     data plane WebSocket (/api/v1/stream)
internal/dispatch/   admission by deadline, FIFO queue, load metrics
internal/record/     JSON Lines recording of events (docs/RECORDING.md)
internal/worker/     gRPC client, catalog and supervisor of the Python worker
internal/workerpb/   generated gRPC code (from proto/)
proto/               the server ↔ worker contract
worker/              Python worker (TFLite segments over gRPC), sciot-model build, decoders
client/              Python device client (docs/CLIENT.md) and its benchmarks
testdata/            golden files: the executable specification of the decoders and the model hash
```

Dependencies point one way (`cmd/server` wires everything):

```text
config    → api, load, registry, server, stream, worker
server    → api, device, dispatch, load, record, registry, stream
worker    → api, stream, workerpb      (implements stream.Executor over gRPC)
dispatch  → stream                     (an Executor in front of the worker)
stream    → api, record                (sessions and executor are interfaces)
record    → api, load
registry, device → api
api, load → nothing internal
```
