# SCIoT_go_server

Go server for SCIoT distributed inference. It is the **control plane**: it
pairs edge devices, knows the models they run, collects their telemetry and
tells each device whether to run the model fully on the device (`LOCAL`) or
split it with the server (`SPLIT`).

The inference loop on the device never waits for the server: it reads the
current policy locally, and a background heartbeat (1 Hz by default) keeps
policy and telemetry in sync. The protocol is described in
[docs/PROTOCOL.md](docs/PROTOCOL.md).

Status (phase 1):

- [x] registration, model information, heartbeat with windowed telemetry,
  versioned `LOCAL`/`SPLIT` policy, session expiry
- [x] opt-in admin endpoint to force a policy (experiments)
- [ ] data plane (activation upload, remote partition execution); runtime
  decided in [docs/spikes/ML_RUNTIME.md](docs/spikes/ML_RUNTIME.md)
- [ ] offloading algorithm (the policy is static until then)

The reference client is `client/python/http_go_client.py` in
[SCIoT_python_client](https://github.com/UBICO/SCIoT_python_client).

## Requirements

Go 1.27 or newer. One external dependency: gopsutil, for the server load metrics.

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
  "models": [
    {
      "model_key": "fomo_96x96",
      "tflite_dir": "/path/to/models/fomo_96x96/layers/tflite",
      "last_offloading_layer": 58,
      "input_size_bytes": 18432
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
| `models[].tflite_dir` | — | directory with the `*.tflite` segments; their MD5 is the model hash |
| `models[].last_offloading_layer` | — | index of the last segment |
| `models[].input_size_bytes` | — | size of the raw RGB565 input |

Unknown keys are rejected, and the server refuses to start if a model
directory has no segments or two models have the same segments.

## Running

```bash
go run ./cmd/server -config config.json -addr :8080
```

Flags `-verbose` and `-admin` switch the corresponding options on (they
cannot switch them off: use `config.json` for that). With `-admin`, bind to
localhost: `-addr 127.0.0.1:8080`.

Force a policy on a registered device (admin enabled):

```bash
curl -X POST http://127.0.0.1:8080/api/v1/admin/devices/web_96/policy -d '{"mode":"SPLIT","split_point":5}'
```

## Development

```bash
gofmt -l . && go vet ./... && go test -race ./...
```

Layout:

```text
cmd/server/          main: flags, wiring, http.Server with timeouts
internal/api/        protocol types and their validation (no dependencies)
internal/config/     config.json → registry.Config, server.Config
internal/registry/   model_hash → model info, computed at startup
internal/device/     in-memory device state (sessions, policy, telemetry)
internal/load/       server host load, sampled in its own goroutine (gopsutil)
internal/server/     HTTP handlers, verbose heartbeat table
```

Dependencies point one way: `cmd/server → config → server → registry, device → api`.
