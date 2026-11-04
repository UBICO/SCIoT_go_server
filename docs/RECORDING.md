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
 "dispatch":{"service_tau_ms":10000,"window_s":10},
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
 "network":{"bandwidth_bps":11250000,"bandwidth_std_bps":820000,"bandwidth_age_ms":420,"latency_ms":1.8,"not_limiting":false,"valid_samples":412,"discarded_samples":37},
 "device_segments":[{"mean":0.47,"std":0.02,"n":12,"age_ms":3},"…",null]}
```

- `applied` is the policy the device declares, `server_policy` the server's
  after the heartbeat: they differ while a change is in flight.
- `telemetry` is the device's window since its previous heartbeat (see
  PROTOCOL.md).
- `queue` is the dispatcher's state, absent without the data plane.
- `network` is the device's network seen by the server (see [Network](#network)),
  absent if the device never opened a stream.
- `device_segments` is the server's estimate of the device's compute time
  per segment (ms), from the `segment_ms` of the heartbeats so far: mean and
  standard deviation of the window means, number of windows, age; `null`
  for a segment the device never ran. Absent if the device sends no
  `segment_ms`. See [Estimates](#estimates).
- `model_key`, `num_segments`: the device's model. `service` is the
  dispatcher's `S` per split point of that model (`split_point`, `mean_ms`,
  `std_ms`, `samples`, `source`), for every split point it can estimate:
  `source` is `measured` (frames at that split point) or `segments` (sum of
  the segments still to run plus the model's overhead, measured on other
  split points or by the [profile](#profile--the-server-profiled-a-model));
  `std_ms` and `samples` only for `measured`;
  `input_bytes` the bytes a device sends at each split point (`split_point`,
  `input_bytes`), from the worker; `max_inflight` the maximum of the
  device's window (the hello's). With
  these a replay rebuilds the cost model's inputs (`sciot-replay algo`).

### `policy` — the server changed a device's policy

`device_id`, `session_id`, `policy` (the new one, with its window
`max_inflight` when it has one), `reason`: `admin`, or the algorithm's
`latency` (better beyond the margin) or `infeasible` (the current candidate
broke the constraints); see [OFFLOAD.md](OFFLOAD.md).

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
  only valid samples enter the moving average (`bandwidth_bps`, with
  `bandwidth_std_bps` and `bandwidth_age_ms`; time-weighted, τ
  `stream.bandwidth_tau_ms`, default 60 s: see [Estimates](#estimates)).
- **The limit.** The server sees bytes when it reads them, not when they
  arrive. Bytes that piled up in the TCP buffer (the server was busy, or the
  network is faster than the server reads) make `receive_ms` too short and
  the bandwidth too high: that is why such samples are discarded, and counted
  in `discarded_samples`. If fewer than 10% of the samples of the last 60 s
  are valid (at least 3 samples), `not_limiting` is true: the network is not the bottleneck (localhost, a
  fast LAN) and `bandwidth_bps` must not be read as a limit.
- **Latency.** A WebSocket ping every `stream.ping_interval_ms` (default
  1000), its pong awaited up to 5 s: `latency_ms` is half the *minimum* RTT
  of the pings of the last 60 s. Not an average: the device's pong travels
  on the same link as its frames, and with the link saturated it waits
  behind the bytes already in the device's TCP send buffer (bufferbloat:
  ~170 ms at 50 Mbit/s, measured with
  `client/scripts/bench_network.py`), so those pings measure the queue, not the
  latency. The long window keeps a value measured while the link was free
  (LOCAL, startup, pauses) through a period of saturation; after more than
  60 s of continuous saturation the latency includes the queue.
- **Probe.** In `LOCAL` no frames flow. To a device that declared the
  capability `probe` the server sends a `probe_request` when no message has
  been measured for `stream.probe_interval_ms` (default 30 s; 5 s,
  `stream.probe_unknown_interval_ms`, while its bandwidth has no estimate,
  so that "not limiting" can be reached on a link that never waits); the device's
  answer (`stream.probe_bytes`, default 512 KiB) is measured like a frame
  (docs/PROTOCOL.md, "Probe"). The stream stays open in `LOCAL`, so pings
  also run on a free link there.
- WebSocket compression stays disabled: with it, the bytes on the network
  would not be the bytes of the message.

The state belongs to the device, not to the stream: it survives reconnects
and new sessions.

### Estimates

Every moving average of the server (service time `S` per split point,
segment and model overhead; bandwidth; the device's time per segment) is the
same estimator (`internal/ewma`): mean, standard deviation of the samples,
number of samples and age, **weighted on time**. A sample arriving `Δt`
after the previous one weighs

    α = max(1 − e^(−Δt/τ), 1/(n+1))

so the estimate forgets the past at the same speed in seconds whatever the
rate of the samples: a sample `τ` old weighs 1/e of a new one. With rare
samples (a probe every 30 s) each one weighs a lot, with dense ones (50
frames/s) each weighs little. The `1/(n+1)` term makes the first samples a
plain mean: a burst at startup (the server's profile, the first frames)
does not leave the first, often slowest, sample as the estimate for `τ`.

| Estimate | τ | Option |
|---|---|---|
| `S` per split point, segment, overhead | 10 s | `dispatch.service_tau_ms` |
| bandwidth | 60 s | `stream.bandwidth_tau_ms` |
| device time per segment | 10 s | `server.device_segment_tau_ms` |

The deviation is of the samples as they arrive: per frame for `S`, per
sample for the bandwidth, per heartbeat window for the device's segments
(how much the window mean changes, not the jitter between frames). Whoever
uses them (phase 3's algorithm) decides how; the metric does not.

### `probe` — a device answered a probe_request

```json
{"kind":"probe","ts":"…","t_ms":91240.1,"device_id":"web_96","session_id":"YVOF…",
 "probe_id":3,"bytes":524288,"receive_ms":41.9,"receive_waited_share":0.97,"receive_valid":true}
```

Same reception fields as a `frame`. Only answers the server accepted (right
`probe_id` and size) are recorded.

### `profile` — the server profiled a model

```json
{"kind":"profile","ts":"…","t_ms":2210.4,"model_key":"yolo_small","runs":5,
 "segment_ms":[3.1,2.4,"…",19.8],"overhead_ms":4.2,"duration_ms":212.7}
```

When the worker becomes ready (at start and after every restart) the server
runs each whole model (split −1) `dispatch.profile_runs` times, after one
discarded warm-up, on a zero input, without decoder. The runs seed the
dispatcher's time per segment and the model's overhead, so `S` is known for
every split point before a device uses it (`source: segments`). They wait
for a free slot like frames but are not traffic: they do not enter λ, ρ,
interarrivals or `S` per split point, and are counted in `totals.profiled`.
`segment_ms` and `overhead_ms` are the means of the measured runs; `error`
says why a profile stopped (the runs before it are in the estimates).

The overhead is measured with the largest tensor (the raw input) and no
decoder: a frame's own measures replace it as they arrive.

### `decision` — an offloading algorithm's choice at a heartbeat

```json
{"kind":"decision","ts":"…","t_ms":81234.5,"device_id":"web_96","algorithm":"static",
 "current":{"mode":"SPLIT","split_point":6,"policy_version":2},
 "mode":"SPLIT","split_point":6,"changed":false,"reason":"static",
 "predictions":[{"split_point":6,"window":3,"local":false,"total_ms":43.53,"device_ms":17.03,"upload_ms":0.04,
   "wait_ms":0,"service_ms":26.42,"suffix_ms":45.27,"server_ms":43.53,"local_ms":62.3,
   "frames_per_s":52.4,"server_frames_per_s":52.4,"server_share":1,"bound":"device",
   "p95_ms":44.45,"rho":0.69,"saturated":false},"…"]}
```

Written by the server at every heartbeat when an algorithm is enabled
(`offload.algorithm`), and by `sciot-replay algo -out`, in the same format.
`reason` says why the decision is what it is (`latency`, `infeasible`,
`best`, `hysteresis`, `min_hold`, `current_unknown`, `no_candidate`,
`few_samples`, `static`; see [OFFLOAD.md](OFFLOAD.md)). `max_inflight` is
the chosen window, absent for `LOCAL` and for a policy without one.
`pending_per_s` and `pending_erlangs` are the load other devices reserved
and the dispatcher does not measure yet, added to the others' before
predicting ([OFFLOAD.md](OFFLOAD.md#reservations)); omitted when 0.
`predictions` has one entry per candidate, a pair split point and `window`
(split −1..last−1, window from the maximum down to 1), and LOCAL last
(no `window`), with the fields of [COSTMODEL.md](COSTMODEL.md): how many frames per
second the device makes and the server serves, the share served, what bounds
it, the latency of a served frame (`server_ms`) and of one the device
finishes (`local_ms`), the overall mean (`total_ms`) and 95th percentile
(`p95_ms`), the frames per second refused by the server or that found the
window full (`rejected_per_s`, `window_full_per_s`, omitted when 0).
Infinite or undefined values (no frame served) are `null`, and `missing`
lists the inputs a prediction lacked.

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
