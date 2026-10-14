# Spike 2.2 — ML runtime for the remote partitions

Status: **decided** (2026-10-14), to be confirmed on the lab machine.
Code: branch [`spike/ml-runtime`](https://github.com/UBICO/SCIoT_go_server/tree/spike/ml-runtime/spike/ml-runtime)
(commit `03ded0d`). It is a throw-away prototype and is not merged; this document is.

## Question

In `SPLIT` the server must run the remaining segments of the model on the
activation sent by the device. How should the Go server execute TFLite
segments, given that:

- the server runs on **Linux x86_64** in the lab (development on macOS arm64);
- it must serve **several devices at the same time**;
- the per-request overhead adds to the latency that decides whether offloading
  is worth it at all.

## Decision

1. **Option B**: the Go server orchestrates; a **Python worker** runs the
   segments with `tf.lite.Interpreter`, called over **gRPC**. The Go binary
   stays pure Go (`GOOS=linux GOARCH=amd64 go build` from any machine); the
   worker uses the official TensorFlow wheels, already supported on all the
   platforms of the Python repository.
2. **One worker process with a pool of slots**, a slot being a complete set of
   interpreters used by one request at a time. It scales like one process per
   slot with less than half the memory: TFLite releases the GIL during
   `invoke()`.
3. **gRPC over TCP on `127.0.0.1`**, not a Unix socket: faster in this
   measurement (see S4) and identical inside containers.
4. **Slots and TFLite threads per slot are configuration**, not constants.
   Start with slots × threads ≈ cores and few slots (e.g. `cores/4` slots of 4
   threads); tune on the lab machine.
5. **The queue lives in the Go server**: a dispatcher limits in-flight requests
   to the number of slots and measures the wait. The wait and the in-flight
   count are edge-load signals for the offloading algorithm (phase 3).

## Options considered

| | Option | Outcome |
|---|---|---|
| A | TFLite C API in the Go process via cgo | **Dropped.** `libtensorflowlite_c` is not shipped by pip or Homebrew: it must be built from the TensorFlow sources with CMake for every platform (macOS for development, Linux x86_64 in the lab), the Go binary needs cgo and is no longer static, cross-compiling needs a C toolchain per target. |
| A' | ONNX Runtime via cgo (prebuilt libraries) | Not evaluated: it keeps cgo and adds a `.tflite → .onnx` conversion to validate for every segment. |
| **B** | **Python TFLite worker over gRPC** | **Chosen**, measured below. |

## Setup

- Machine: Apple M4 Pro, 12 cores (8 performance + 4 efficiency), 24 GB, macOS 15.7.
- Software: Go 1.27.1, grpc-go 1.84.0; Python 3.12.11, tensorflow 2.20.0, grpcio 1.84.0, numpy 2.5.
- Model: yolo-small 320×320, 8 float32 segments. Activation size at each split
  point (the payload Go → worker):

  | After segment | Shape | Bytes |
  |---|---|---|
  | input | 320×320×3 | 1.2 MB |
  | 0 | 160×160×32 | 3.2 MB |
  | 1–3 | 80×80×64 | 1.6 MB |
  | 4–6 | 40×40×128 | 0.8 MB |
  | output | 2100×6 | 50 KB |

- The server runs the chain from the split point to the end: **k = 0** (3.2 MB,
  7 segments, worst case) and **k = 4** (0.8 MB, 3 segments).
- Fixed-seed input; 20 warm-up requests excluded, 200 measured. Latency is
  measured in the Go client, from the tensor in Go memory to the output in Go
  memory. Every output is compared with the in-process result: the maximum
  absolute difference is **0** in all runs.
- Baseline (variant 0): the same chain in the Python process, no IPC,
  2 TFLite threads: **32.8 ms** (k = 0), **27.8 ms** (k = 4).

## Results

### S1/S4 — cost of one request through gRPC

One request at a time, 1 slot, 2 threads. S4 repeats the comparison 5 times
with fresh workers and alternating order.

| | k = 0 (3.2 MB) | k = 4 (0.8 MB) |
|---|---|---|
| in-process (baseline) | 32.8 ms | 27.8 ms |
| gRPC, TCP 127.0.0.1 | 37.0 ± 0.1 ms (**+4.3**) | 30.0 ± 0.1 ms (**+2.2**) |
| gRPC, Unix socket | 40.3 ± 0.1 ms (+7.5) | 30.8 ± 0.1 ms (+3.0) |

- The overhead grows with the payload, ~1 ms per MB over TCP.
- The Unix socket is slower in every repetition (+3.2 ms at 3.2 MB, +0.8 ms at
  0.8 MB, spread 0.1 ms). Not verified why; the likely cause is the gRPC Python
  transport (buffers or HTTP/2 flow-control windows), not the OS. **To recheck
  on Linux.**

### S2 — concurrent devices: pool of threads vs pool of processes

k = 0, 2 TFLite threads per slot. Throughput in requests/s; p95 latency seen by
the client, queue wait included.

| Layout | 1 client | 2 | 4 | 8 | Memory |
|---|---|---|---|---|---|
| 1 slot | 25 | 28 | 28 | 28 (p95 286 ms) | 0.7 GB |
| threads, 2 slots | 25 | 44 | 55 | 55 | 0.9 GB |
| threads, 4 slots | 25 | 45 | 76 | **100** (p95 83 ms) | **1.1 GB** |
| processes, 2 | 24 | 44 | 53 | 52 | 1.2–1.4 GB |
| processes, 4 | 24 | 43 | 72 | 92 (p95 111 ms) | **2.5 GB** |

- One slot serializes every device: beyond ~28 req/s requests only queue.
- The thread pool scales as well as (slightly better than) the process pool
  with less than half the memory.

### S3/S5 — slots × TFLite threads

S5 uses all 12 cores (slots × threads = 12), k = 0, with as many clients as
slots and with twice as many (queue always full).

| Slots × threads | clients = slots: req/s | mean | p95 | clients = 2 × slots: req/s | mean | p95 | Memory |
|---|---|---|---|---|---|---|---|
| 1 × 12 | 35 | **28.5 ms** | 30 | 42 | 47 | 49 | 0.7 GB |
| 2 × 6 | 67 | 29.9 ms | 31 | 83 | 48 | 52 | 0.8 GB |
| 3 × 4 | 78 | 38.4 ms | 41 | 91 | 65 | 72 | 1.0 GB |
| 4 × 3 | 82 | 48.8 ms | 53 | 100 | 79 | 92 | 1.1 GB |
| 6 × 2 | 85 | 70.0 ms | 77 | 101 | 117 | 148 | 1.3 GB |
| 12 × 1 | 102 | 115.8 ms | 149 | **107** | 216 | 296 | 2.2 GB |

S3 (2 slots, 4 clients): 1 thread 30 req/s, 2 threads 55, 4 threads 87.

- Peak throughput is ~100 req/s whatever the split of the cores, once there
  are ≥ 4 slots: it is the CPU limit. More slots do not add capacity.
- The trade-off is **latency per request vs throughput**: few slots with many
  threads keep each request close to (or below) the 2-thread baseline; many
  slots serve more devices at once, each one slower. For offloading the
  latency of a single request matters (`SPLIT` must beat local execution), so
  **2 × 6 or 3 × 4** are the best starting points here.
- Memory: ~100–120 MB per slot.

## Implications for the next steps

- **2.3 data plane contract.** The device → server hop dominates, not the local
  Go → worker hop: the same 3.2 MB take ~500 ms on a 50 Mbit/s link versus ~4 ms
  locally. Reducing the activation (float16 halves it, int8 quantization
  divides by 4) is the main lever, and makes the choice of the split point a
  network choice too. Activations travel as raw bytes; shape and dtype come
  from the model manifest.
- **gRPC message size.** The default 4 MiB limit is raised to 64 MiB on both
  sides; it must stay configurable for larger models.
- **2.4 partition manager.** Dispatcher in Go: bounded concurrency equal to the
  slots (buffered-channel semaphore), FIFO wait, timeout, `inflight` and
  `queue_wait_ms` metrics, per-segment server times (returned by the worker as
  `layer_ms`), worker health and restart. When the queue grows the server can
  answer "busy" and the device runs that frame `LOCAL`.
- **Possible optimization**, only if the lab shows a larger overhead: shared
  memory between Go and worker (same machine) to remove the gRPC copy.

## Caveats

- Numbers come from a laptop. Absolute values will differ on the lab machine
  (x86_64, different core count and frequencies); the relative conclusions
  (thread pool vs process pool, latency/throughput trade-off) are what this
  spike decides.
- The M4 Pro has 8 performance and 4 efficiency cores; configurations using
  all 12 threads run partly on the slower cores.
- Memory is the RSS of the worker process tree, including the `uv` launcher
  (a few MB).

## Still open

- Run the same scenarios on the lab machine (Linux x86_64): a package with the
  Go client built for `linux/amd64`, the worker in a container and the run
  script, so that whoever has access can run it with one command.
- Recheck Unix socket vs TCP on Linux.

## Reproducing

```bash
git switch spike/ml-runtime
cd spike/ml-runtime
MODEL_DIR=/path/to/yolo-small ./run_spike.sh                      # scenarios 0-3
SCENARIOS="0 4 5" MODEL_DIR=/path/to/yolo-small ./run_spike.sh    # S4 and S5
```

Results are written to `out/results.jsonl` (one JSON object per measurement).
