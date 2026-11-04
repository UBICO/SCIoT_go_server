# Offloading algorithm — choosing each device's policy

`internal/offload` decides, at every heartbeat of a device, its policy:
`LOCAL`, or `SPLIT` at a split point with a window (`max_inflight`, the
frames in flight at most). The algorithm adapts to the metrics the
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

1. **Predictions**: for every candidate, the cost model
   ([COSTMODEL.md](COSTMODEL.md)): a formula for `LOCAL`, the simulation of
   device and dispatcher for `SPLIT`. A candidate is `LOCAL` or a pair
   (split point −1..last−1, window 1..`stream.max_inflight`): 9 × 3 + 1 for
   yolo-small. The load of the other devices is the measured one plus what
   the algorithm granted and the dispatcher does not see yet
   ([Reservations](#reservations)). A prediction with missing inputs is not
   a candidate; if the current one's is missing, nothing changes
   (`current_unknown`): one cannot tell whether another is better.
2. **Minimum duration**: no change before the policy has been in force for
   `min_hold_ms` (`min_hold`). The estimates of a new split point need time:
   `S`, the wait and the device's cycle change with it.
3. **Constraints**: a candidate is admitted only if the device makes at
   least `min_fps_ratio` × the frames per second of `LOCAL` (offloading
   must not slow the device down), and at most `max_reject_share` of its
   frames are refused by the server (each costs the device the whole
   suffix). `LOCAL` is always admitted with the default ratio 1.
4. **Objective**: the admitted candidate with the lowest `p95_ms`
   (default) or `total_ms` (`objective: mean`); at equal value, the one with
   more segments on the device, then the smaller window (less load on the
   server; [Ties](#ties)). Why both exist:
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

   If the current candidate violates the constraints, the algorithm moves
   to the best without margin (`infeasible`); the minimum duration still
   applies. A best with fewer than 3 noise samples cannot replace the
   current one (`few_samples`).

A change of split point or of window gives the device a new policy version,
in the answer to that same heartbeat and on the stream; the recording gets a
`policy` event with the reason (`latency` or `infeasible`) and, at every
heartbeat, a `decision` event with every prediction, the reserved load it
saw and the reason.

### Ties

At equal value the candidate that comes later in the predictions wins:
`SPLIT` from split −1 up, each with its windows from the maximum down to 1,
then `LOCAL`. So a tie goes to `LOCAL`, then to the higher split point, then
to the smaller window: more of the model on the device, less load on the
server.

With the `p95` objective ties are frequent, not chance: when more than 5%
of a `SPLIT` candidate's frames are finished by the device (full window,
refused), those frames cost the whole model, and the p95 *is* the `LOCAL`
time. Two devices on one server (CLIENT.md, "Multi-device benchmark"), the
second deciding with the first at split 6:

| Candidate | p95 (ms) | Mean (ms) | Served by the server | Frames/s |
|---|---|---|---|---|
| `LOCAL` | 66.0 | 66.0 | 0% | 15.0 |
| split 1, window 1 | 66.0 | 54.8 | 50% | 28.0 |
| split 6, window 2 | 114.7 | 69.1 | 61% | 27.0 |

The p95 cannot see that half the frames of split 1 are 11 ms faster on
average and the frames per second double. This is accepted (decided
2026-11-04): the objective is the stability of the latency, which does not
get worse, and frames per second are a constraint, not a goal; staying
`LOCAL` also leaves the server to the other devices.

The tie rule alone would not change this: a tie is a gain of 0, never
beyond the margin, so the device keeps its policy whatever the rule (the
rule matters only when the current candidate breaks the constraints and the
algorithm moves without margin). The alternative, for phase 3.8 with
several devices: the mean as a second criterion, with its own margin, when
the p95 are within the margin; it trades a better mean for more load on
the server.

### Window

The window decides how many frames the device sends and how many it
finishes itself (full window: the client never waits), so how much load it
brings. The right one depends on the split point, and the simulation
predicts it: on the single-device recordings (yolo-small, 2 slots):

| Split | Window | Predicted p95 | Frames/s |
|---|---|---|---|
| −1 | 3 | 133 ms | 29.6 |
| −1 | 2 | 67 ms | 44.4 |
| 6 | 3 | 48 ms | 55.2 |
| 6 | 2 | 48 ms | 55.2 |

At −1 three frames in flight on two slots queue behind the device's own
frames; at 6 two are enough, because a frame holds the window only after it
is sent (upload, wait, S, return: ~29 ms against a cycle of ~18 ms), so the
tie goes to the smaller window. `stream.max_inflight` is the maximum: the
hello carries it, the server refuses only frames beyond it, and the
`static` algorithm and the admin without `max_inflight` leave it in force.
Later: a maximum computed per device from its network, with a cap in
`config.json` for memory.

### Reservations

The dispatcher measures λ and ρ over a sliding window of 10 s, so a device
that moves to `SPLIT` appears in the measure gradually. Without correction,
another device deciding a second later sees the server almost free, moves
too, and both overload it. When the algorithm moves a device it reserves
the difference of server load between the new candidate and the current one
(frames served per second × S, in erlang). The next decisions of the other
devices add the part the measure does not contain yet,
`reserved × max(0, 1 − age / 10 s)`: no load is counted twice or never.

- Only increases are reserved: a device that leaves `SPLIT` frees load the
  measure sees with the same delay, and meanwhile the others are only more
  careful.
- Reading the reservations, deciding and reserving happen under one lock:
  two heartbeats at the same instant do not see the same free load.
- Simplification: the reservation is made when the algorithm decides, even
  if the server then does not apply the change (device pinned or registered
  again meanwhile, rare); it fades within 10 s.

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
device: the algorithm does not touch it. Its window is `max_inflight` if
given, otherwise the maximum. `DELETE` on the same path gives it
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

One simulation per `SPLIT` candidate per heartbeat: ~0.1 ms each, about
3 ms per heartbeat for yolo-small with a maximum window of 3 (27
candidates), in the heartbeat handler. Decisions of different devices run
one at a time (the reservations' lock).
shortcut: synchronous; with many devices, move it to a goroutine and apply
the decision at the next heartbeat.
