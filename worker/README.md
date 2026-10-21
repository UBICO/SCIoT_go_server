# sciot-worker

The Python side of the SCIoT Go server, one uv project with two commands:

| Command | What |
|---|---|
| `sciot-worker` | runs the TFLite segments of `SPLIT` frames over gRPC and decodes the output into detections; started and supervised by the Go server |
| `sciot-model build` | turns Edge Impulse exports into a model directory (segments + `manifest.json`), see [docs/MODELS.md](../docs/MODELS.md) |

Both use the same TensorFlow version (`uv.lock`): segments are built and run
by the same runtime. Design and measurements of the worker:
[docs/spikes/ML_RUNTIME.md](../docs/spikes/ML_RUNTIME.md). The contract with
the Go server is `proto/sciot_worker/v1/worker.proto`.

```text
sciot_worker/server.py          gRPC service: Describe, RunFrom (slots, deadlines, decoding)
sciot_worker/models.py          model directory, model hash, segment chain
sciot_worker/decoders/          model output → detections (yolo_pro)
sciot_worker/build/             sciot-model build: split, Edge Impulse export, manifest
scripts/make_decoder_golden.py  real golden files for the decoders
```

## Running the worker

The Go server normally starts the worker itself, with the models of its
`config.json`. To run it by hand:

```bash
uv run sciot-worker --listen 127.0.0.1:50051 --slots 2 --threads 6 \
  --model yolo_small_320=/path/to/yolo-small
```

A *slot* is a complete set of interpreters for every configured model, used by
one request at a time. slots × threads should not exceed the cores.

`RunFrom` runs a model from `split_point + 1` to the last segment. When the
request carries `postprocess` (the Go server always sends the model's), the
worker decodes the output after giving the slot back and returns the
detections; the raw output only with `want_raw`.

## Building a model

```bash
uv run sciot-model build <savedmodel.zip> --ei-cpp-export <cpp.zip> --sample image.png --output ~/sciot-models/yolo_small_320
```

Which exports to download, the checks, the manifest and the model hash
warning: [docs/MODELS.md](../docs/MODELS.md).

## Decoders

`sciot_worker/decoders` decodes `SPLIT` outputs; the devices decode `LOCAL`
outputs with their own implementation of the same algorithm (Python client,
ESP32). They are kept identical by the golden files in
[testdata/decoders](../testdata/decoders): hand-written ones, one rule each,
and real yolo-small outputs written by

```bash
uv run python scripts/make_decoder_golden.py ~/sciot-models/yolo_small_320 image.jpg ../testdata/decoders/yolo_pro/real_name --rule "what it checks"
```

The specification of each decoder, and how to add a family, is in
[docs/MODELS.md](../docs/MODELS.md), "Decoders".

## Tests

```bash
uv run pytest
```

The tests build tiny TFLite models at run time and check the decoders against
the golden files: no model artifacts needed.

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
