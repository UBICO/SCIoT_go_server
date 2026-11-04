# Offloading algorithm — choosing each device's policy

`internal/offload` decides, at every heartbeat of a device, its policy:
`LOCAL` or `SPLIT` at a split point. The algorithm adapts to the metrics the
server already measures (see [RECORDING.md](RECORDING.md), "Estimates"); it
asks for no new ones.

| `offload.algorithm` | What |
|---|---|
| `static` (default) | never changes the policy: only the admin does; no prediction is computed |
| `latency` | below |

The same code runs live in the server and in `sciot-replay algo`, on the
same state: `offload.StateFrom` builds it from the heartbeat the server
records, so live and replay cannot diverge.

## `latency`

At every heartbeat, in order:

1. **Predictions**: for every split point −1..last, the cost model
   ([COSTMODEL.md](COSTMODEL.md)): a formula for `LOCAL`, the simulation of
   device and dispatcher for `SPLIT`. A prediction with missing inputs is not
   a candidate; if the current split point's is missing, nothing changes
   (`current_unknown`): one cannot tell whether another is better.
2. **Minimum duration**: no change before the policy has been in force for
   `min_hold_ms` (`min_hold`). The estimates of a new split point need time:
   `S`, the wait and the device's cycle change with it.
3. **Constraints**: a split point is admitted only if the device makes at
   least `min_fps_ratio` × the frames per second of `LOCAL` (offloading
   must not slow the device down), and at most `max_reject_share` of its
   frames are refused by the server (each costs the device the whole
   suffix). `LOCAL` is always admitted with the default ratio 1.
4. **Objective**: the admitted split point with the lowest `p95_ms`
   (default) or `total_ms` (`objective: mean`); at equal value, the one with
   more segments on the device. Why both exist:
   [COSTMODEL.md](COSTMODEL.md#mean-or-p95).
5. **Hysteresis**: the best replaces the current one only if it is better
   by more than a margin (`hysteresis` otherwise):

       margin = max(margin_min_ms, margin_rel × current) × (1 + k_queue · max(0, ρ − queue_rho_from)/(1−ρ))  +  k_noise · σ

   - `ρ` is the best candidate's utilization from the simulation (capped at
     0.99): near saturation a small error on the load moves the latency a
     lot, so the margin grows. Only above `queue_rho_from` (default 0.8):
     below, the queue model predicts the wait well (COSTMODEL.md,
     verification 1: close below ρ ≈ 0.8, 2–4× off above), and a margin that
     grew from low loads kept a single device at split 3 instead of split 6,
     26% faster at ρ = 0.69;
   - `σ` is the noise of the two predictions' difference (below).

   If the current split point violates the constraints, the algorithm moves
   to the best without margin (`infeasible`); the minimum duration still
   applies. A best with fewer than 3 noise samples cannot replace the
   current one (`few_samples`).

A change gives the device a new policy version, in the answer to that same
heartbeat and on the stream; the recording gets a `policy` event with the
reason (`latency` or `infeasible`) and, at every heartbeat, a `decision`
event with every prediction and the reason.

### Noise σ

For each device and split point the algorithm remembers a time-weighted
estimate (`internal/ewma`, τ = 10 s) of the **differences between
consecutive predictions**: σ = std(differences) / √2. A level change (the
server becomes faster or slower) weighs a single difference, so σ does not
grow when the algorithm should react; noise weighs every time. A missing
prediction restarts the series. The memory is in the server's process: it
is forgotten when the device registers again and when the server restarts,
and every replay starts with a new algorithm.

### Reactivity

The algorithm compares the latest predictions, whose inputs are already
time-weighted estimates (τ = 10 s). After a clear change of performance
it reacts in about 5–20 s: the inputs, the minimum duration, the margin. To
keep the inputs fresh for split points no frame uses, the server profiles
a model again when the estimate of one of its segments is older than
`dispatch.reprofile_age_ms` (default 30 s): with a device at split k,
segments 0..k never run on the server.

## Admin

A policy set with `POST /api/v1/admin/devices/{id}/policy` **pins** the
device: the algorithm does not touch it. `DELETE` on the same path gives it
back to the algorithm (the policy stays until the algorithm decides
otherwise). A new registration starts unpinned.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `offload.algorithm` | `static` | `static` or `latency` |
| `offload.objective` | `p95` | `p95` or `mean` |
| `offload.min_fps_ratio` | 1 | 0–10 |
| `offload.max_reject_share` | 0.1 | 0–1 |
| `offload.min_hold_ms` | 10000 | 0–3600000 |
| `offload.margin_min_ms` | 2 | 0–1000 |
| `offload.margin_rel` | 0.1 | 0–1 |
| `offload.k_queue` | 1 | 0–100; 0 removes the queue term (ablation, phase 3.8) |
| `offload.k_noise` | 1 | 0–100; 0 removes the noise term |
| `offload.queue_rho_from` | 0.8 | 0–0.99: utilization from which the queue term grows; 0 = from no load |

`sciot-replay algo -algorithm latency -config config.json` replays a
recording with the parameters of that file's `offload` section.

## Cost

One simulation per `SPLIT` point per heartbeat: ~0.1 ms each, under 1 ms per
heartbeat for yolo-small, in the heartbeat handler.
shortcut: synchronous; with many devices, move it to a goroutine and apply
the decision at the next heartbeat.
