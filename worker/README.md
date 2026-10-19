# sciot-worker

Python worker that runs the TFLite segments for the SCIoT Go server, over
gRPC. Design and measurements: [docs/spikes/ML_RUNTIME.md](../docs/spikes/ML_RUNTIME.md).
The contract is `proto/sciot_worker/v1/worker.proto`.

The Go server normally starts the worker itself, with the models of its
`config.json`. To run it by hand:

```bash
uv run sciot-worker --listen 127.0.0.1:50051 --slots 2 --threads 6 \
  --model yolo_small_320=/path/to/yolo-small
```

A *slot* is a complete set of interpreters for every configured model, used by
one request at a time. slots × threads should not exceed the cores.

## Tests

```bash
uv run pytest
```

The tests build tiny TFLite models at run time: no model artifacts needed.

## Regenerating the gRPC code

After editing the `.proto`, from the repository root (needs `protoc-gen-go`
and `protoc-gen-go-grpc` in `PATH`):

```bash
uv run --project worker python -m grpc_tools.protoc -I proto \
  --go_out=. --go_opt=module=github.com/UBICO/SCIoT_go_server \
  --go-grpc_out=. --go-grpc_opt=module=github.com/UBICO/SCIoT_go_server \
  --python_out=worker --grpc_python_out=worker \
  sciot_worker/v1/worker.proto
```
