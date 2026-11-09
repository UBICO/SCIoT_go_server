# CLAUDE.md

SCIoT split-computing server in Go. A device runs the first segments of a
TFLite model, the server runs the rest; at every heartbeat an offloading
algorithm picks each device's policy (LOCAL, or SPLIT at a split point with a
window of frames in flight). Protocol v1: `docs/PROTOCOL.md`.

## Layout
- `cmd/server` — the server. `cmd/sciot-replay` — offline replay of a recording
  (algorithms, queue model, client CSV check).
- `internal/`:
  - `api` protocol v1 types and validation; `server` HTTP endpoints, wiring,
    decider goroutine; `device` in-memory device store; `config` `config.json`;
  - `stream` data plane (one WebSocket per device, `/api/v1/stream`);
    `dispatch` admission, queue and service time between stream and worker;
    `worker` gRPC client and supervisor of the Python worker; `workerpb` generated;
  - `offload` algorithms; `costmodel` predictions (event simulation of client
    and dispatcher); `netmeasure` bandwidth/latency measured by the server;
    `load` host load sampler; `ewma` time-weighted estimates;
  - `record` JSONL recording (`docs/RECORDING.md`); `replay` reads it back;
    `registry` models by `model_hash`.
- `proto/` — gRPC contract with the worker.
- `worker/` — Python inference worker (uv project); `sciot-model build` makes
  model directories (`docs/MODELS.md`).
- `client/` — Python device client (uv project `sciot-client`, `docs/CLIENT.md`,
  Raspberry setup in `docs/RASPBERRY.md`); benchmarks in `client/scripts/`,
  results in `client/results/` (gitignored).
- `testdata/` — goldens shared by Go, worker and client (`model_hash`, decoders).
- `docs/` — design and verification: COSTMODEL, OFFLOAD, PROTOCOL, RECORDING, MODELS.

## Run
```bash
go run ./cmd/server -config config.json -addr 127.0.0.1:8080 -admin
uv run --project client sciot-client --server http://127.0.0.1:8080 --model-dir <model dir>
```
`config.json` is local (gitignored); start from `config.example.json`.

## Checks (same as CI, `.github/workflows/test.yml`)
- Go: `gofmt -l .` must print nothing; `go vet ./...`; `go test -race ./...`.
- Worker, from `worker/` (CI Python 3.12): `uv sync --frozen`;
  `uvx ruff@0.17.1 check .`; `uvx ruff@0.17.1 format --check sciot_worker tests`;
  `uv run pytest -v`.
- Client, from `client/` (CI Python 3.11): `uv sync --frozen`;
  `uvx ruff@0.17.1 check .`; `uvx ruff@0.17.1 format --check sciot_client tests scripts`;
  `uv run pytest -v`. Tests marked `external_server` need a running server
  and `SCIOT_MODEL_DIR`; they are skipped by default.
- Client Python 3.11–3.13 only: TensorFlow 2.20 has no 3.14 wheels.
- Use ruff at the version CI pins, not a local one.

## Conventions
- Go comments are in Italian; Python code, docs, commits and PRs are in English.
- Branch first, never commit on `main`. PRs to `main` of `UBICO/SCIoT_go_server`.
- Never commit `config.json`, `spike/`, `worker/tune-*.json`.
- A protocol change updates server, client and `docs/PROTOCOL.md` in the same PR.
- Metrics never depend on an algorithm; algorithms use the metrics that exist.
- `model_hash` (MD5 of the `*.tflite` files in name order) is computed in Go,
  worker and client: change all three together with `testdata/model_hash`.
- "Absent" is not zero: unmeasured values stay null/unset, never 0.
- After editing a `.md`, grep the other docs for statements it made stale.
