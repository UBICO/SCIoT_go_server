# Recording — `sciot-record.v1`

The server can record what happens to a file, to evaluate offline what
phase 3 (the offloading algorithm) needs: replay an algorithm on the same
data, compare the dispatcher's predictions with what was measured, plot load
against latency.

Off by default. Enable it in `config.json`:

```json
"record": {"dir": "~/sciot-records"}
```

Every server start writes a new file `sciot-<YYYYMMDD-HHMMSS>.jsonl` (local
time) in that directory: one experiment, one file. The path and the counters
are in `GET /api/v1/server/status` under `record`.

## Format

JSON Lines: one JSON object per line. Every line has:

| Key | Meaning |
|---|---|
| `kind` | the event type, below |
| `ts` | UTC time of the event (RFC 3339): to correlate with logs and devices |
| `t_ms` | milliseconds since the server started, on a monotonic clock: use it for durations, it does not jump with NTP corrections |

The time is when the event happened, not when it was written.

### `start` — always the first line

```json
{"kind":"start","ts":"…","t_ms":0,"schema":"sciot-record.v1","heartbeat_interval_ms":1000,
 "dispatch":{"service_ewma_alpha":0.2,"window_s":10},
 "models":[{"model_hash":"b182…","model_key":"yolo_small_320","num_layers":8,"last_offloading_layer":7,
            "input":{…},"postprocess":{…}}]}
```

`dispatch` is absent without the data plane. The file describes itself: no
need for the `config.json` of the run.

### `session` — a device registered

`device_id`, `session_id`, `device_type`, `model_key`, `capabilities`.

### `heartbeat` — what a decision would see at that moment

```json
{"kind":"heartbeat","ts":"…","t_ms":81234.5,"device_id":"web_96","session_id":"YVOF…",
 "applied":{"mode":"LOCAL","policy_version":1},
 "server_policy":{"mode":"LOCAL","split_point":null,"policy_version":1},
 "telemetry":{"inference_count":47,"inference_time_ms":21.3,"inference_time_max_ms":38.9,"cpu_load":0.42,"rtt_ms":3.1},
 "since_last_seen_ms":1001.2,
 "server_load":{"cpu_percent":18.2,"cpu_count":10,"load_avg_1_min":2.1,"memory_available_bytes":8517640192,"goroutines":41},
 "queue":{"slots":2,"running":1,"queued":0,"offered_per_s":9.8,"admitted_per_s":9.8,"utilization":0.31,"predicted_wait_ms":0},
 "network":{"bandwidth_bps":11250000,"latency_ms":1.8,"not_limiting":false,"valid_samples":412,"discarded_samples":37}}
```

- `applied` is the policy the device declares, `server_policy` the server's
  after the heartbeat: they differ while a change is in flight.
- `telemetry` is the device's window since its previous heartbeat (see
  PROTOCOL.md).
- `queue` is the dispatcher's state, absent without the data plane.
- `network` is the device's network seen by the server (see [Network](#network)),
  absent if the device never opened a stream.

### `policy` — the server changed a device's policy

`device_id`, `session_id`, `policy` (the new one), `reason` (`admin`; phase 3
will add the algorithm's reasons).

### `frame` — the outcome of a SPLIT frame, admitted or not

```json
{"kind":"frame","ts":"…","t_ms":81240.1,"device_id":"web_96","session_id":"YVOF…",
 "sequence_id":1042,"policy_version":2,"split_point":5,"request_bytes":819200,"deadline_ms":45,
 "receive_ms":72.4,"receive_waited_share":0.98,"receive_valid":true,
 "outcome":"result","admission":{"predicted_wait_ms":3.1,"service_ms":8.2,"known":true},
 "timing":{"receive_ms":72.4,"queue_wait_ms":2.8,"server_ms":11.4,"layer_ms":[1.1,0.9,2.0]},"detections":1}
```

| Key | Meaning |
|---|---|
| `outcome` | `result`, `server_busy` (not admitted), `deadline_exceeded` (expired in the queue), `worker_unavailable` |
| `admission` | the dispatcher's prediction: queue wait and service time `S` it used, and whether `S` came from measures. Present whenever the dispatcher saw the frame: `result`, `server_busy`, `deadline_exceeded` (expired in the queue or while running in the worker), and a `worker_unavailable` that happened during execution. Absent only when no worker was ready at arrival. Comparing it with `timing` shows how wrong the prediction was; a `deadline_exceeded` is by definition a wrong one |
| `timing` | measured times, as in the result sent to the device; `result` only |
| `detections` | how many objects were found; `result` only. Boxes are not recorded |
| `deadline_ms` | the frame's deadline from when the server read it |
| `receive_ms`, `receive_waited_share`, `receive_valid` | how long the server took to read the frame, the share of its bytes that came from reads that waited for the network, and whether the sample entered the bandwidth estimate (see [Network](#network)). For every outcome: the network carried the frame anyway |

`ts` and `t_ms` of a `frame` are when its outcome was known (sent to the
device), not when it arrived: for a `result`, the arrival is
`t_ms − timing.server_ms`.

### Network

The server measures each device's network on its stream, with no help from
the device and one clock per duration:

    upload time of a frame ≈ latency + bytes / bandwidth

- **Bandwidth.** The server reads every frame in fixed 64 KiB reads and
  times each one. A read that took at least 200 µs waited for the network;
  a faster one copied bytes already in the kernel's TCP buffer. A frame is a
  *valid* sample if at least 90% of its bytes came from reads that waited:
  only valid samples enter the moving average (`bandwidth_bps`, α 0.2).
- **The limit.** The server sees bytes when it reads them, not when they
  arrive. Bytes that piled up in the TCP buffer (the server was busy, or the
  network is faster than the server reads) make `receive_ms` too short and
  the bandwidth too high: that is why such samples are discarded, and counted
  in `discarded_samples`. If fewer than 10% of the last 20 frames are valid,
  `not_limiting` is true: the network is not the bottleneck (localhost, a
  fast LAN) and `bandwidth_bps` must not be read as a limit.
- **Latency.** A WebSocket ping every `stream.ping_interval_ms` (default
  1000): `latency_ms` is half the *minimum* RTT of the last 10 pings. Not an
  average: the device's pong travels on the same connection as its frames
  and waits behind one being sent, so those pings measure the upload, not
  the latency.
- WebSocket compression stays disabled: with it, the bytes on the network
  would not be the bytes of the message.

The state belongs to the device, not to the stream: it survives reconnects
and new sessions.

### `end` — the last line, at shutdown

`written` (lines before this one), `dropped` (events discarded, see below).
A file without `end` comes from a server that did not shut down cleanly.

## Guarantees

- **Recording never slows requests.** Events go through a buffer to a
  dedicated writer. If the buffer is full (slow disk), the event is
  discarded and counted in `dropped`, never waited for.
- **At most the last second is lost** if the server crashes: the file is
  flushed every second.
- **Not recorded**: tensors, detection boxes, anything a device did not send.

## Reading it

```python
import pandas as pd

events = pd.read_json("sciot-20261021-101500.jsonl", lines=True)
frames = pd.json_normalize(events[events.kind == "frame"].to_dict("records"))
frames["wait_error_ms"] = frames["timing.queue_wait_ms"] - frames["admission.predicted_wait_ms"]
```
