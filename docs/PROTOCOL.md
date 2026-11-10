# SCIoT protocol — v1

This document is the contract between the SCIoT Go server and its clients
(the Python reference client today, the ESP32-S3-EYE later). It has two parts:

- the **control plane** (HTTP + JSON): pairing, model information, telemetry
  and the execution policy (`LOCAL` / `SPLIT`);
- the **data plane** (WebSocket, see [Data plane](#data-plane)): activation
  tensors sent by devices in `SPLIT` and the results of the remote partition.

The source of truth for field names is `internal/api`; the tests in
`internal/api/types_test.go` pin the exact JSON.

## Conventions

- Transport: HTTP/1.1 with keep-alive. The server keeps idle connections open
  for 120 s, so a client sending a heartbeat every second reuses one TCP
  connection.
- Bodies are JSON (`Content-Type: application/json`). Unknown keys in a
  request are **rejected** (`400 invalid_request`): a typo never goes unnoticed.
- Request bodies are limited to 64 KiB.
- Every error has the same shape:

  ```json
  {"error": {"code": "model_not_found", "message": "unknown model_hash \"...\""}}
  ```

  Clients must branch on `code`; `message` is for humans and may change.

| `code` | Status | Meaning |
|---|---|---|
| `invalid_request` | 400 | malformed JSON, unknown key, body too large, missing or invalid field |
| `unsupported_protocol` | 400 | `protocol_version` is not 1 |
| `model_not_found` | 404 | the server does not serve this `model_hash` |
| `session_expired` | 409 | the device or session is not (or no longer) known |
| `device_not_found` | 404 | admin endpoint: the device is not registered |
| `internal_error` | 500 | server bug; details are in the server log only |

## Model identity

A model is identified by `model_hash`: the **MD5 of the concatenated contents
of all `*.tflite` segment files, ordered by file name** (lexicographic, so
`submodel_10` sorts before `submodel_2`; what matters is that client and
server use the same order). The client computes it on its local segments;
the server computes it at startup for every model in `config.json`. Matching
hashes guarantee that both sides split the same network at the same layers.

## Execution policy

```json
{"mode": "LOCAL", "split_point": null, "policy_version": 1}
{"mode": "SPLIT", "split_point": 5,    "policy_version": 2}
{"mode": "SPLIT", "split_point": 5,    "policy_version": 3, "max_inflight": 2}
```

- `mode`: `LOCAL` (whole model on the device) or `SPLIT` (layers
  `0..split_point` on the device, the rest on the server).
- `split_point`: always present; `null` in `LOCAL`, an integer in
  `-1..last_offloading_layer - 1` in `SPLIT`: the last segment the device
  runs, as in frames. `-1` means the device runs nothing and sends the model
  input (everything on the server); `last_offloading_layer - 1` leaves only
  the last segment to the server. `last_offloading_layer` is not a `SPLIT`
  (nothing would be left to the server): that is `LOCAL`.
- `max_inflight`: optional, only in `SPLIT`: the device's window, frames in
  flight at most, `1..` the hello's `max_inflight`. Absent = the hello's
  value. A change of window is a new policy version, like a change of split
  point.
- `policy_version`: increases every time the server changes the policy.
  It is **per session**: a new registration starts again from `LOCAL` v1.

Client rules:

1. Apply a received policy only if its `policy_version` is **greater** than
   the one already accepted. Equal or older versions are ignored (late or
   duplicated answers).
2. A policy takes effect from the **next inference**: an inference already
   started finishes with the policy it started with.
3. Keep "accepted policy" and "executed policy" separate. When the server is
   unreachable, or the client cannot execute `SPLIT` (no data plane yet), it
   executes `LOCAL` but keeps the accepted version. Overwriting the accepted
   policy with `LOCAL` would make the client ignore the same version forever
   once the server is back.

## Endpoints

### `POST /api/v1/register`

Pairs a device. A registration of an already known `device_id` **replaces**
its whole state (new session, policy back to `LOCAL` v1).

Request:

```json
{
  "protocol_version": 1,
  "device_id": "web_96",
  "device_type": "python",
  "model_hash": "2b319af7002697e06bf876905d72d6a5",
  "capabilities": {"probe": true}
}
```

`capabilities` is what the client's code can do (see
[Capabilities](#capabilities)); it may be omitted.

`200`:

```json
{
  "session_id": "YVOFM5HEJSRDMW2JBQU4ADGDD6",
  "model": {"model_key": "yolo_small_320", "num_layers": 8,
            "last_offloading_layer": 7,
            "input": {"shape": [1, 320, 320, 3], "dtype": "float32"},
            "postprocess": {"decoder": "yolo_pro", "labels": ["bicycle", "stop sign"],
                            "score_threshold": 0.5, "iou_threshold": 0.2,
                            "max_detections": 200}},
  "policy": {"mode": "LOCAL", "split_point": null, "policy_version": 1},
  "heartbeat_interval_ms": 1000,
  "data_plane": true
}
```

`data_plane` says whether the server has the stream (`GET /api/v1/stream`).
When true the device opens it right after registering, in any mode: in `LOCAL`
it carries no frames, but pings (latency), `policy` messages and probes
(bandwidth). When false the device uses the control plane only.

`model` comes from the model's `manifest.json` ([MODELS.md](MODELS.md)):
`input` is the tensor the device prepares (shape with batch 1, data type);
`postprocess` is how the device decodes the output into detections in
`LOCAL`. The server decodes `SPLIT` results with the same parameters, so a
result reads the same whoever computed it. `iou_threshold` is absent for
decoders without NMS.

Errors: `400 invalid_request`, `400 unsupported_protocol`, `404 model_not_found`.
On `model_not_found` the client should run LOCAL-only and not retry.

#### Capabilities

A capability says the client can do something the server may ask for. Rules:

- The client's **code** declares them, once, at registration. A configuration
  may only withdraw one (e.g. a device on a metered link turning off
  `probe`), never declare one the code lacks.
- **Absent means no.** The server never asks for what was not declared.
- The server **ignores capabilities it does not know**, so a client newer than
  the server still registers. Everywhere else in a request an unknown field is
  `400 invalid_request`.
- Static for the session: state that changes (load, memory) is telemetry.

| Capability | Meaning |
|---|---|
| `probe` | the client answers `probe_request` on the stream ([Probe](#probe)) |
| `wait_for_window` | with its window full, the client waits for a place when that costs less than finishing the frame itself ([CLIENT.md](CLIENT.md#window-full)); without it, it finishes the frame at once. The server's cost model simulates the rule the client declares |

`raw_input_split` (sent by earlier clients) is no longer defined: every client
can be told split `-1`; servers ignore it.

### `POST /api/v1/heartbeat`

Sent every `heartbeat_interval_ms` (from the registration answer). Carries the
telemetry of the **window** since the previous heartbeat and the policy the
client is executing (acknowledgement). The answer always contains the current
server policy.

Request:

```json
{
  "device_id": "web_96",
  "session_id": "YVOFM5HEJSRDMW2JBQU4ADGDD6",
  "policy_version": 1,
  "mode": "LOCAL",
  "telemetry": {
    "inference_count": 47,
    "inference_time_ms": 21.3,
    "inference_time_max_ms": 38.9,
    "cpu_load": 0.42,
    "rtt_ms": 3.1,
    "memory_available_bytes": 812345344,
    "segment_ms": [0.5, 2.7, 0.5, 3.8, 2.7, 0.6, 6.8, null],
    "segment_count": [47, 47, 47, 47, 47, 47, 47, 0]
  }
}
```

- `policy_version` / `mode`: the policy the client is **executing**, not the
  last one received (they differ during a fallback or when `SPLIT` is
  downgraded).
- Telemetry:

  | Field | Required | Rule |
  |---|---|---|
  | `inference_count` | yes | inferences completed in the window, `>= 0`; `0` means the inference loop is stalled |
  | `inference_time_ms` | only if count > 0 | mean compute time of the window; absent if count is 0 |
  | `inference_time_max_ms` | only if count > 0 | `>= inference_time_ms` |
  | `cpu_load` | no | `0..1`, average over the window |
  | `rtt_ms` | no | duration of the **previous** heartbeat; absent after a failed one |
  | `memory_available_bytes` | no | `>= 0` |
  | `segment_ms` | no, with `segment_count` | per segment `0..last` of the model, the mean compute time of the window on the device; `null` for a segment not run in the window |
  | `segment_count` | with `segment_ms` | per segment, how many times it ran in the window, `>= 0`; `0` if and only if `segment_ms` is `null` there |
  | `wait_ms` | no | total time the client waited in the window for a place in its window of frames in flight (see [CLIENT.md](CLIENT.md#window-full)), `>= 0`; absent = `0` (a client that never waits) |
  | `answer_ms` | no | mean of `rtt − receive_ms − server_ms` (the data plane's [timing](#data-plane)) over the window's results of frames that travelled alone (no other frame of the device in flight when sent, none sent before the answer): latency of the first byte up, return of the answer and the client's time to receive it, `>= 0`; absent = no such result in the window (the server keeps its estimate). The other frames also waited on the link behind an earlier one, which the cost model simulates itself, or had their answer delayed by the client sending the next one. A server before phase 4.2 rejects the field: update server and client together |

  Optional fields are omitted when unknown: `0` is a value, not "absent".
  `segment_ms` and `segment_count` have one entry per segment of the
  session's model (`num_layers`), or `400 invalid_request`. A client that
  sends them sends them in every heartbeat, all `null`/`0` when no segment
  ran (split −1 with every frame served): the server derives the rest of the
  device's cycle from them, and a missing window is "not measured", not
  "nothing computed". They are the raw
  window, like the rest of the telemetry: the server keeps the moving
  averages; with them it estimates the device's cost of a split it has
  not used. A segment counts every run, the prefix of a frame and the
  completion of a refused one.
  `NaN` and `Infinity` are not valid JSON and must never be sent.

`200`:

```json
{"policy": {"mode": "LOCAL", "split_point": null, "policy_version": 1}}
```

Errors: `400 invalid_request`, `409 session_expired`. On `session_expired`
(server restarted, or a newer registration of the same device) the client
falls back to `LOCAL` and registers again.

### `GET /api/v1/models`, `GET /api/v1/models/{model_hash}`

Models served by this server, sorted by `model_key`:

```json
{"models": [{"model_hash": "b182...", "model_key": "yolo_small_320", "num_layers": 8,
             "last_offloading_layer": 7,
             "input": {"shape": [1, 320, 320, 3], "dtype": "float32"},
             "postprocess": {"decoder": "yolo_pro", "labels": ["bicycle", "stop sign"],
                             "score_threshold": 0.5, "iou_threshold": 0.2,
                             "max_detections": 200}}]}
```

The single-model form returns one entry, or `404 model_not_found`.

### `POST /api/v1/admin/devices/{device_id}/policy` (opt-in)

Forces the policy of a registered device; it is delivered with the next
heartbeat answer. Enabled only with `-admin` or `"admin": true` in
`config.json`, and **unauthenticated**: bind the server to localhost when
using it.

```json
{"mode": "SPLIT", "split_point": 5}
{"mode": "SPLIT", "split_point": 5, "max_inflight": 2}
```

The server assigns the version (current + 1) and validates `split_point`
against the device's model (`-1..last_offloading_layer - 1`) and
`max_inflight` against the stream's maximum (`1..stream.max_inflight`). `200`:

```json
{"device_id": "web_96", "session_id": "YVOF...",
 "policy": {"mode": "SPLIT", "split_point": 5, "policy_version": 2}}
```

Errors: `400 invalid_request`, `404 device_not_found`.

The policy **pins** the device: the offloading algorithm (when enabled,
[OFFLOAD.md](OFFLOAD.md)) does not change it.

### `DELETE /api/v1/admin/devices/{device_id}/policy` (opt-in)

Releases the pin: the algorithm decides the device's policy again from its
next heartbeat. The policy itself does not change. `200` with the same body
as above (the current policy); `404 device_not_found`.

### `GET /api/v1/server/status`

Load of the server machine (sampled every `load_sample_interval_ms`), plus
counters. Like `/healthz` it is for monitoring and experiments, not for clients.
Load fields are omitted when they could not be measured (absent ≠ 0).

```json
{"load": {"cpu_percent": 9.2, "cpu_count": 12, "load_avg_1_min": 1.45,
          "memory_total_bytes": 25769803776, "memory_available_bytes": 7240630272,
          "goroutines": 5, "sampled_at": "2026-10-14T15:56:39.81+02:00"},
 "devices": 0, "models": 2,
 "started_at": "2026-10-14T15:56:37.81+02:00", "uptime_seconds": 2.0}
```

### `GET /healthz`

`200 {"status":"ok"}`. Not versioned: it is for process monitoring, not for
clients.

## Typical session

```text
client                                   server
  │ POST /register (model_hash)            │
  │──────────────────────────────────────▶│ session S, LOCAL v1
  │◀──────────────────────────────────────│
  │                                        │
  │ every interval: POST /heartbeat        │
  │   (S, applied LOCAL v1, window stats)  │
  │──────────────────────────────────────▶│
  │◀──────────── policy (LOCAL v1) ────────│
  │              ...                       │
  │                                        │ policy changes → SPLIT@5 v2
  │ POST /heartbeat (applied LOCAL v1)     │
  │──────────────────────────────────────▶│
  │◀──────────── policy (SPLIT@5 v2) ──────│
  │ next inference uses SPLIT@5            │
  │              ...                       │
  │                                        │ server restarts
  │ POST /heartbeat (S)                    │
  │──────────────────────────────────────▶│
  │◀──────────── 409 session_expired ──────│
  │ fall back to LOCAL, register again     │
```

## Data plane

In `SPLIT` the device runs segments `0..k` and sends the activation (the output
of segment `k`) to the server, which runs `k+1..last` and returns the output of
the model. The data plane carries those tensors; everything else stays on the
control plane.

### Design principles

- **Asynchronous.** A device may send frame `N+1` before the result of `N`
  arrives. Results can come back **out of order**; the device matches them by
  `sequence_id`.
- **Bounded.** Each device has at most `max_inflight` frames in flight: the
  policy's window, or the hello's `max_inflight` (the maximum) when the
  policy has none. A smaller window leaves the frames already in flight
  alone: the device sends no more until fewer are. The server refuses only
  frames beyond the maximum (`inflight_limit`). The server queue has **no
  fixed length**: a frame is admitted only if it can be served within its
  deadline (see
  [Admission](#admission)); otherwise it is rejected with `server_busy` and the
  device runs **that frame** `LOCAL`. A frame is never retried: by the time it
  could be retried, it is old.
- **Frames carry their own split point.** The policy is decided by the server,
  but a frame is computed with the policy in force when the device started it.
  The server runs each frame from **its** `split_point + 1`, even if the policy
  has changed in the meantime: the device has already executed `0..k` and that
  work cannot be moved. `policy_version` attributes the measurement to the
  policy that produced it.
- **Self-contained results.** A result follows one schema,
  `inference-result.v1`, whoever computed it (the device in `LOCAL`, the server
  in `SPLIT`) and whatever carries it (the stream, an MQTT topic). It contains
  the **interpreted** output (detections), so consumers do not need the model.
- **Tensors travel as raw bytes.** Shape and dtype are implied by the model and
  the split point (the worker reports them at startup), so they are never sent.

### Connection

```http
GET /api/v1/stream HTTP/1.1
Upgrade: websocket
X-SCIoT-Device: web_96
X-SCIoT-Session: YVOFM5HEJSRDMW2JBQU4ADGDD6
```

The device opens **one** stream after registering (when the registration
answer has `data_plane: true`), with the `device_id` and `session_id` of its
current session, and keeps it open in every mode. Before the upgrade the server answers
with the usual JSON errors: `409 session_expired` (unknown session),
`400 invalid_request` (missing header), `503 worker_unavailable` (no data plane
for this model yet). If the device opens a second stream, the first one is
closed (code `4002`).

Right after the upgrade the server sends a `hello` message:

```json
{"type": "hello", "max_inflight": 3, "max_message_bytes": 4194304,
 "policy": {"mode": "SPLIT", "split_point": 5, "policy_version": 2},
 "segments": [{"split_point": -1, "input_bytes": 1228800},
              {"split_point": 0,  "input_bytes": 3276800}, "..."]}
```

`segments` lists, for every valid split point, the exact size of the activation
the server expects (from the worker's `Describe`).

### Message format

WebSocket gives message boundaries; inside a message the layout is:

- **text messages** (control): a JSON object with a `type` field;
- **binary messages** (data): a small JSON header followed by the raw tensor.

```text
binary message
┌──────────────────────┬────────────────────────┬──────────────────────────┐
│ header_length (u32,  │ header: JSON object,   │ payload: raw tensor      │
│ little-endian)       │ UTF-8, header_length B │ (rest of the message)    │
└──────────────────────┴────────────────────────┴──────────────────────────┘
```

A JSON header (~150 bytes against 0.8–3.2 MB of tensor) keeps the format easy
to extend and to parse on every client (cJSON on the ESP32) without a custom
binary layout to version.

| `type` | Direction | Kind | Purpose |
|---|---|---|---|
| `hello` | server → device | text | limits, current policy, expected tensor sizes |
| `frame` | device → server | binary | an activation to run from `split_point + 1` |
| `result` | server → device | binary | an `inference-result.v1` for a frame; the raw tensor only if requested |
| `error` | server → device | text | a frame was not executed |
| `policy` | server → device | text | the policy changed (fast path; the heartbeat stays the reliable path) |

### `frame` (device → server)

```json
{"type": "frame", "sequence_id": 1042, "policy_version": 2,
 "split_point": 5, "encoding": "float32",
 "captured_at": "2026-10-19T10:15:03.120Z", "deadline_ms": 45.0, "want_raw": false}
```

| Field | Rule |
|---|---|
| `sequence_id` | uint64, starts at 1, strictly increasing within a session; unique among frames in flight |
| `policy_version` | version of the policy the frame was computed with |
| `split_point` | last segment executed by the device: `-1` (raw model input) .. `last_offloading_layer - 1` |
| `encoding` | `float32` in v1; `rgb565` reserved for `split_point = -1` (6× smaller than `float32`); other data types (quantized tensors, any precision) reserved for later |
| `captured_at` | wall-clock time (NTP-synchronized) at which the frame was acquired; copied into the result |
| `deadline_ms` | optional: how long the result is still useful, from when the server reads the frame. The natural value is the time the device would need to finish the frame `LOCAL`: a later result is worse than running locally |
| `want_raw` | optional, default `false`: also return the raw output tensor |
| payload | exactly the `input_bytes` announced in `hello` for that `split_point` |

### `result` (server → device)

The header is an `inference-result.v1` (see [Inference result](#inference-result))
with `"type": "result"`. The payload is the raw output tensor (`float32`) if the
frame asked for it with `want_raw`, otherwise empty.

`timing.server_ms` is the time from the moment the frame was read to the
moment the result was ready, queue included; the device obtains the network
time as its own round-trip time minus `server_ms`. `timing.receive_ms` is how
long the server took to read the frame, from its first to its last byte: the
upload time without the latency, possibly underestimated (bytes already
buffered by TCP; see RECORDING.md, "Network"). The server pings the device
on the stream (WebSocket ping, every `stream.ping_interval_ms`): the device's
WebSocket library must answer pings, which every standard one does while it
reads. `server_policy_version` is
the policy version of the server now: if it is newer than the device's, a
policy change is on its way (as a `policy` message or with the next
heartbeat).

### `error` (server → device)

```json
{"type": "error", "sequence_id": 1042, "code": "server_busy",
 "message": "predicted wait 61 ms + service 8 ms > deadline 45 ms"}
```

| `code` | Meaning | Device action |
|---|---|---|
| `server_busy` | not admitted: the predicted completion exceeds the deadline | run this frame `LOCAL` |
| `deadline_exceeded` | admitted, but the deadline expired while waiting: dropped without running | run this frame `LOCAL` (it is usually already done) |
| `inflight_limit` | the device exceeded the hello's `max_inflight` | run this frame `LOCAL`, respect the window |
| `invalid_frame` | bad header, unknown `split_point` or `encoding`, wrong payload size, duplicate `sequence_id` | bug: log it, run `LOCAL` |
| `worker_unavailable` | the worker failed or timed out | run this frame `LOCAL` |

Errors about a frame never close the stream.

### Admission

The server does not cap the queue at a fixed length. The queue lives in the
Go server (FIFO, shared by all devices) and never sends the worker more frames
than its slots, so the server knows exactly what is waiting and what is
running. When a frame arrives it computes its **predicted completion time**:

```text
predicted_wait = when the first slot frees up, after every queued frame
                 has taken the earliest free slot for its own S
predicted      = predicted_wait + S
```

- `c`: slots of the worker (requests it can run at the same time), from
  `Describe`;
- each running frame frees its slot after its remaining service time
  (`S − elapsed`, at least 0); a free slot is free now;
- `S`: mean service time for that model and split point, a time-weighted
  moving average of the measured execution time without queue (see the
  Go repo's `docs/RECORDING.md`, "Estimates"; `GET /api/v1/server/status`
  also gives its standard deviation, samples and age). Frames of different
  models or split points have different `S`, which is why the wait is
  simulated slot by slot instead of using a single `S`. With the same `S` for
  every frame it reduces to `(queued + running − c + 1) / c × S`;
- a split point never measured: if every segment it would run on the server
  has been measured (for other split points), `S` is the sum of the segments'
  mean `layer_ms` plus the model's mean overhead (measured `S` minus the
  segments: gRPC, tensor copy). The server profiles every model when its
  worker becomes ready, so this is the normal case (Go repo's
  `docs/RECORDING.md`, "profile"). Otherwise the longest known `S` is used for
  the other frames' predictions, and the frame itself is admitted only if a
  slot is free (its first run gives the first measure).

The frame is admitted if `predicted ≤ deadline_ms`, rejected with
`server_busy` otherwise. A frame without `deadline_ms` uses a server-side
default (configurable). An admitted frame whose deadline expires while it waits
is dropped with `deadline_exceeded`, so no slot is spent on a result nobody
will use.

The admitted queue length therefore adapts by itself (Little's law,
`L = λ · W`): it shrinks when the service slows down, grows when slots are
added. The offered and admitted arrival rates λ, the mean service time `S` per
model and split point, the measured utilization ρ (busy slot time / (c × window))
and the predicted wait are exposed in `GET /api/v1/server/status` (`dispatch`). The
offloading algorithm (phase 3) uses them in a simulation of the device and
of this queue, with this same admission rule, to predict how the wait would
change before moving a device to or from `SPLIT` (Go repo's
`docs/COSTMODEL.md`); admission itself needs no model.

### `policy` (server → device)

```json
{"type": "policy", "policy": {"mode": "LOCAL", "split_point": null, "policy_version": 4}}
```

Sent when the policy of the device changes. The device applies it with the
same rules as a policy received in a heartbeat (only if newer).

### Probe

The server measures each device's network on the stream (see the Go repo's
`docs/RECORDING.md`, "Network"): bandwidth from how long it takes to read a
message, latency from WebSocket pings. Frames are the measurement in `SPLIT`;
in `LOCAL` no bytes flow, so the server may ask for a probe:

```json
{"type": "probe_request", "probe_id": 7, "bytes": 524288}
```

The device answers with a **binary** message, same layout as a frame (see
[Message format](#message-format)), header

```json
{"type": "probe", "probe_id": 7}
```

and a payload of exactly `bytes` bytes, any content (zeros). The server
measures it like a frame and does not run it.

- Only to devices that declared the capability `probe`.
- When the measurement is old: no message measured for
  `stream.probe_interval_ms` (default 30 s), or for
  `stream.probe_unknown_interval_ms` (default 5 s) while the device's
  bandwidth has no estimate. On a link where reads never wait (localhost, a
  fast LAN) no probe is a valid bandwidth sample, and the upload is known
  only as "not limiting", which needs 3 samples in 60 s: one probe every
  30 s would never get there. A device that keeps sending frames is never
  asked. One probe at a time; one unanswered for that long is replaced by a
  new one.
- A `probe` with an unknown `probe_id` or the wrong size is answered with an
  `error` (`invalid_frame`, `sequence_id` 0); the probe it should have
  answered stays open.
- The device answers from its receiving side, without waiting for its
  inference loop: a late answer measures the device, not the network.

### Closing

| Close code | Meaning | Device action |
|---|---|---|
| `1001` | server shutting down | `LOCAL`, reconnect with back-off |
| `1009` | message larger than `max_message_bytes` | bug: log it |
| `4001` | session expired (the device registered again, or the server lost the session) | `LOCAL`, register again, then reconnect |
| `4002` | replaced by a newer stream of the same device | none (the newer stream is the one in use) |

When the stream is down the device keeps running `LOCAL`; frames in flight at
that moment are considered lost.

### Typical exchange

```text
device                                              server
  │ GET /api/v1/stream (upgrade)                       │
  │───────────────────────────────────────────────────▶│
  │◀──────────── hello (max_inflight 3, SPLIT@5 v2) ───│
  │ frame #1 (split 5)                                 │
  │───────────────────────────────────────────────────▶│ queue → worker
  │ frame #2 (split 5)                                 │
  │───────────────────────────────────────────────────▶│ queue → worker
  │◀────────────── result #2 ──────────────────────────│ (out of order)
  │◀────────────── result #1 ──────────────────────────│
  │                                                    │ policy → LOCAL v3
  │◀────────────── policy (LOCAL v3) ──────────────────│
  │ (next frames run LOCAL, nothing more is sent)      │
```

### Inference result

`inference-result.v1` is the single schema of an inference outcome, used on
the stream and, later, on MQTT. It is produced by whoever computed the output:
the **device** for `LOCAL` frames, the **server** for `SPLIT` frames.

```json
{"schema": "inference-result.v1",
 "device_id": "web_96", "session_id": "YVOF…", "sequence_id": 1042,
 "model_key": "yolo_small_320", "mode": "SPLIT", "split_point": 5,
 "policy_version": 2, "server_policy_version": 3, "computed_by": "server",
 "captured_at": "2026-10-19T10:15:03.120Z",
 "timing": {"device_ms": 18.4, "queue_wait_ms": 0.1, "server_ms": 7.3,
            "layer_ms": [1.1, 0.9, 2.0]},
 "detections": [{"label": "stop_sign", "score": 0.91,
                 "box": {"x": 0.42, "y": 0.18, "w": 0.21, "h": 0.25}}]}
```

| Field | Rule |
|---|---|
| `mode`, `computed_by` | `LOCAL` / `device`, or `SPLIT` / `server` |
| `split_point` | `null` in `LOCAL` |
| `captured_at` | from the frame (NTP wall clock): lets consumers order and correlate results of different devices |
| `timing.device_ms` | compute time on the device (segments `0..k`, or the whole model in `LOCAL`) |
| `timing.receive_ms`, `queue_wait_ms`, `server_ms`, `layer_ms` | `SPLIT` only |
| `detections` | interpreted output: label, confidence, box normalized to `[0, 1]` (x, y = top-left). The post-processing (decoding, thresholds, NMS) is the same on device and server |

On the stream the result also carries `"type": "result"`; `server_policy_version`
is present only in results computed by the server.

### MQTT (planned)

Not implemented in v1; recorded here so that the result format already fits it.

- Topic: `sciot/<device_id>/results`, payload: the `inference-result.v1` JSON
  (detections only, no raw tensor).
- Publisher: whoever computed the output. The device publishes its `LOCAL`
  results, the server publishes the `SPLIT` results; no extra traffic is added
  to carry `LOCAL` results to the server.
- Consumers (applications, dashboards, loggers) read one stream per device
  regardless of where each frame was computed; `computed_by` tells them.
