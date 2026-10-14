# Spike 2.2 — ML runtime: Go server + Python TFLite worker over gRPC

Throw-away prototype to decide how the Go server runs the remote partitions
(option B: the Go process orchestrates, a Python worker runs TFLite). Only the
results document is meant to be kept; this code is not part of the server.

Questions it answers:

1. What does one request cost through gRPC, compared with running the same
   segments in-process (variant 0)? Unix socket vs TCP. Split 0 (3.2 MB
   activation, 7 segments) and split 4 (0.8 MB, 3 segments).
2. How does the worker scale with concurrent devices: 1–8 concurrent clients,
   pool of interpreter sets in one process (threads) vs one process per slot?
3. TFLite threads per interpreter: slots × threads must not exceed the cores.

## Layout

```text
proto/worker.proto        contract Go ↔ worker (generated: workerpb/, worker/worker_pb2*.py)
worker/segments.py        load and run a chain of submodel_*.tflite
worker/worker.py          gRPC worker; a "slot" = one complete set of interpreters
worker/gen_inputs.py      fixed-seed inputs, expected outputs, in-process baseline
client/                   Go client: concurrent requests, latency stats, correctness
run_spike.sh              every scenario → out/results.jsonl
```

This directory is a separate Go module (its own `go.mod`): the gRPC
dependencies do not reach the server's `go.mod`, and `go test ./...` at the
repository root does not include it.

## Running

Requirements: Go, `uv` (the worker runs in a throw-away environment with
tensorflow 2.20.0, grpcio and numpy). Results land in `out/` (ignored).

```bash
MODEL_DIR=/path/to/yolo-small ./run_spike.sh
```

## Regenerating the gRPC code

Only after editing `proto/worker.proto`. Needs `protoc-gen-go` and
`protoc-gen-go-grpc` in `PATH` (`go install google.golang.org/protobuf/cmd/protoc-gen-go@latest`,
`go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest`); the
`protoc` bundled with grpcio-tools is used.

```bash
uv run --no-project --with grpcio-tools python -m grpc_tools.protoc -I proto \
  --go_out=workerpb --go_opt=paths=source_relative \
  --go-grpc_out=workerpb --go-grpc_opt=paths=source_relative \
  --python_out=worker --grpc_python_out=worker proto/worker.proto
```
