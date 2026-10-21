# sciot-worker

The Python side of the SCIoT Go server, one uv project with two commands:

| Command | What |
|---|---|
| `sciot-worker` | runs the TFLite segments of `SPLIT` frames over gRPC and decodes the output into detections; started and supervised by the Go server |
| `sciot-worker tune` | measures slots × threads on the machine it runs on and recommends the worker size for `config.json` |
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
sciot_worker/tune.py            sciot-worker tune: slots × threads for this machine
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

## Choosing slots and threads: `sciot-worker tune`

The right worker size depends on the server machine, so measure it **on the
server**, with the model it will serve:

```bash
uv run sciot-worker tune --model yolo_small_320=/path/to/yolo_small_320
```

It reads the machine first: logical cores, the cores this process may use
(CPU affinity and cgroup limit on Linux: a container or a shared lab machine
may see fewer cores than it has), physical and performance/efficiency cores,
memory. Then, for every combination that uses the usable cores without
exceeding them (12 cores: 12×1, 6×2, 4×3, 3×4, 2×6, 1×12), it starts a real
`sciot-worker` process, as the Go server does, and loads it over gRPC:

- **1 request at a time**: the latency of one frame, which decides whether a
  `SPLIT` frame beats `LOCAL` within its deadline;
- **as many requests as slots**: the throughput. The Go dispatcher never
  sends the worker more requests than its slots.

It prints a table, the combination with the lowest latency, the one with the
highest throughput, and the **recommended** one: the highest throughput among
those within 10 % of the best latency. Then the `config.json` snippet, and a
JSON file with every number and the machine description, to take away from a
machine you cannot log into. The outputs of all combinations are compared:
the number of threads must not change the results.

Options: `--splits=-1,6` (split points to load; default `-1`, the whole
model; the `=` is needed when the list starts with a negative number),
`--combos 2x6,3x4` (instead of the automatic list), `--seconds` and
`--warmup` (per measurement, default 8 and 2), `--out`. About 2 minutes for
6 combinations.

Example (MacBook, 12 cores: 8 performance + 4 efficiency, yolo-small, whole
model):

| slots × threads | 1 request p50 (ms) | throughput (req/s) |
|---|---|---|
| 12 × 1 | 66.2 | 121.9 |
| 6 × 2 | 36.9 | 119.8 |
| 4 × 3 | 26.2 | 109.7 |
| 3 × 4 | 21.7 | 96.8 |
| 2 × 6 | **18.2** | 84.9 |
| 1 × 12 | 24.6 | 39.5 |

Recommended here: 2 × 6. 1 × 12 is slower than 2 × 6 even per frame: past 6
threads an interpreter also runs on the efficiency cores. These numbers are
of that machine only: run the tool on yours.

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
