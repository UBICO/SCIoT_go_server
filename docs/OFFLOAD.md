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
   yolo-small. The other devices in `SPLIT` are simulated as clients at
   their current policy ([COSTMODEL.md](COSTMODEL.md#frames-the-device-sends-simulation));
   only when one of them has incomplete inputs the simulation falls back to
   their measured load plus what the algorithm granted and the dispatcher
   does not see yet ([Reservations](#reservations)). A prediction with missing inputs is not
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
   [COSTMODEL.md](COSTMODEL.md#mean-or-p95). Then the window (phase 4.2):
   at the best's split point, the admitted window with the most frames per
   second among those within `p95_slack` of the best's value (default:
   up to 25% above it); one replaces another only with at least
   `margin_rel` more frames per second. The split point itself stays chosen
   by the value ([Window](#window)).
5. **Hysteresis**: the best replaces the current one only if it is better
   by more than a margin (`hysteresis` otherwise):

       margin = max(margin_min_ms, margin_rel × current) × (1 + k_queue · max(0, ρ − queue_rho_from)/(1−ρ))  +  k_noise · σ

   - `ρ` is the best candidate's utilization from the simulation (capped at
     0.99): near saturation a small error on the load moves the latency a
     lot, so the margin grows. Only above `queue_rho_from` (default 0.8):
     below, the simulation's p95 is within `margin_rel` of the client's
     (phase 3.8, `sciot-replay check`: [Verification](#verification-phase-38)),
     and a margin that grew from low loads kept a single device at split 3
     instead of split 6, 26% faster at ρ = 0.69;
   - `σ` is the noise of the two predictions' difference (below).

   At the best's split point with another window than the chosen one, the
   hysteresis is on the window instead: the device keeps its window if its
   value is within `(1 + p95_slack)` × the best's plus the margin and the
   chosen window does not give `margin_rel` more frames per second; it moves
   for the frames per second (`fps`) or, beyond the slack, for the value
   (`latency`).

   If the current candidate violates the constraints, the algorithm moves
   to the chosen one without margin (`infeasible`); the minimum duration
   still applies. A chosen candidate with fewer than 3 noise samples cannot
   replace the current one (`few_samples`).

A change of split point or of window gives the device a new policy version,
on the stream as soon as it is decided and in the answer to its next
heartbeat (the answer to the heartbeat that triggered the decision carries
the current policy: see [Cost](#cost)); the recording gets a
`policy` event with the reason (`latency`, `fps` or `infeasible`) and, at every
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
time. With one device at split 6 and window 2, the others see (replay of
the phase 3.8 recordings, medians):

| Candidate | p95 (ms) | Mean (ms) | Served by the server | Frames/s |
|---|---|---|---|---|
| `LOCAL` | 66 | 66 | 0% | 15 |
| split −1, window 2 | 66 | 50 | 67% | 45 |

The p95 cannot see that two frames in three come back 16 ms earlier on
average and the frames per second triple. This is accepted (decided
2026-11-04, confirmed by the test below): the objective is the stability of
the latency, which does not get worse, and frames per second are a
constraint, not a goal; staying `LOCAL` also leaves the server to the
device already in `SPLIT`. Phase 4.2 makes frames per second a goal only
for the window at the split point the p95 chose ([Window](#window)): from
`LOCAL` to split −1 is still a p95 decision, for the reason below (option
b).

The tie rule alone would not change this: a tie is a gain of 0, never
beyond the margin, so the device keeps its policy whatever the rule (the
rule matters only when the current candidate breaks the constraints and the
algorithm moves without margin).

**Option b, tried and dropped (phase 3.8).** The mean as a second criterion:
among the candidates with a p95 within the margin of the current one, the
one with the lowest mean wins if it beats the current mean by more than 10%.
In the replay of 18 recordings such a candidate existed in 59% of the
decisions, nearly all for devices in `LOCAL` (split −1 to 3, window 2:
mean −14…−16 ms, frames per second ×2–3). Live, devices started together,
2 runs each:

| Devices | Rule | p95 per device (ms) | Mean (ms) | Frames/s, all | Policy changes in 2 min |
|---|---|---|---|---|---|
| 2 | p95 only | 47–50 / 69–71 | 46–47 / 68 | 65–66 | 1 |
| 2 | mean as tie-break | 68–86 | 54–57 | 62–63 | 16–18 |
| 4 | p95 only | 51 / 73 | 49 / 71 | 91 | 1 |
| 4 | mean as tie-break | 75–87 | 62–68 | 86–90 | 25–30 |

Every ~10 s (the minimum duration) devices in `LOCAL` moved to a low split
point for the mean, often two or three at once; the server filled up, the
refusals passed 10% and they went back to `LOCAL` (`infeasible`); then
again. The p95 hides the damage: with more than 5% of the frames finished
on the device it stays at the `LOCAL` time however full the server is, so
the candidate is still "within the margin", and only the refusal constraint
stops the move, after it is made. Each device also looks only at its own
mean. The p95 got worse for every device, stability was lost, and the
frames per second and the mean of all devices did not improve. The code was
not kept.

### Window

The window decides how many frames the device sends and how many it
finishes itself (full window: a client without `wait_for_window` never
waits; one with it waits when that is cheaper), so how much load it
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

**Frames per second for the window (phase 4.2, `p95_slack`).** In flight,
frames per second and latency grow together (Little: frames in flight =
frames/s × latency), until a resource saturates. A Raspberry Pi at split −1
sends 1.2 MB a frame over Wi-Fi (~98 ms on the link): window 1 gives 6.8
frames/s at p95 ~170 ms, window 3 11.4 at 271, and window 2, predicted
11.7 at 175–183 against 140–160 for window 1, is the knee. With the p95
alone the algorithm took window 1. So at the split point the value chose,
the window with the most frames per second within `p95_slack` (0.25) of the
best value wins, if it gives at least `margin_rel` (10%) more: below the
simulation's noise (~3%) a few frames per second would buy up to 25% of
p95. Only the window: applied to every candidate, it would repeat option b
([Ties](#ties)), devices leaving `LOCAL` for frames per second at an equal
p95 and filling the server. Replay: every rule-c run of the Pi (with
`answer_ms`) goes to window 2; the phase 3.8 runs with free devices (B, B2)
do not change a single decision. `p95_slack: 0` is the rule before phase
4.2. Applies with `objective: mean` too (the slack is on the value).

### Reservations

With the other devices simulated as clients (phase 3.8) a granted change is
already in the next decisions: the server updates the device's policy in
`offload.Fleet` when it changes it. So reservations are added only in the
fallback, when the others are the measured load; a change still reserves,
for a device that decides in the fallback right after. The ablation (phase
3.8) found no difference with them off when every other device is a client.
The fallback happens when a device in `SPLIT` has incomplete inputs (for
example a client without `segment_ms`, optional in the protocol, or an
unknown bandwidth on a real network): whether reservations can go is to be
measured on real devices (phase 4), counting decisions with `clients` 0
while other devices are in `SPLIT`.

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

## Verification (phase 3.8)

`client/scripts/bench_multi.py` with the algorithm free, yolo-small, 2 slots
× 2 threads, clients and worker on the same MacBook, 120 s after the last
start, 3 runs per scenario (2026-11-06):

| Devices, start | Final policies (every run) | p95 in `SPLIT` / `LOCAL` (ms) | Frames/s, all | Changes | Decision lag p95 / max (ms) |
|---|---|---|---|---|---|
| 2, together | one at 6/2, one `LOCAL` | 47–50 / 69–72 | 65–66 | 1 | 3 / 6 |
| 2, 15 s apart | one at 6/2, one `LOCAL` | 47–54 / 68–75 | 64–66 | 1 | 0.1 / 0.7 |
| 3, together | one at 6/2, two `LOCAL` | 47–50 / 69–71 | 79–81 | 1 | 6 / 12 |
| 3, 15 s apart | one at 6/2, two `LOCAL` | 47–51 / 69–74 | 78–81 | 1 | 0.1 / 4 |
| 4, together | one at 6/2 (once 6/3), three `LOCAL` | 51–60 / 73–81 | 87–91 | 1–2 | 8 / 23 |
| 4, 15 s apart | one at 6/2 (once 6/3), three `LOCAL` | 52–60 / 73–84 | 88–91 | 1–2 | 3 / 11 |

Exactly one device moves to `SPLIT`, after 21–25 s, and stays there; the
others stay `LOCAL`. That is right: with two devices pinned at 6/2 the p95
is 87 ms, worse than `LOCAL` ([COSTMODEL.md](COSTMODEL.md#verification-with-the-client-several-devices-phase-38)).
The two moves from window 2 to 3 (p95 worse by ~8 ms) came with a rise of
the measured `S` (27 → 34 ms) while other clients were loading, and a late
move to `SPLIT` in an early run (fixed since in the benchmark) came from an
`S` profiled while three clients loaded TensorFlow on the same CPU (42 ms,
against 28 measured once in `SPLIT`): CPU contention of the shared machine,
to be checked again with devices on other machines (phase 4).

**Ablation** (2 and 4 devices started together, 2 runs, 90 s): with
`k_noise: 0` or `k_queue: 0` the policies, changes, time to the first
`SPLIT`, p95 and frames per second are those of the full algorithm; with
the other devices simulated as clients the reservations change neither the
predictions nor the decision (`TestReservationsOnlyInTheFallback`). The
decisions here are clear-cut (`SPLIT` beats `LOCAL` by ~20 ms for the first
device and loses by ~20 ms for the second), so the margin's terms never
decide; they stay, for noisier predictions on real networks and devices
that differ (phase 4). `queue_rho_from` from the prediction error per band
of ρ (`sciot-replay check`): no band above `margin_rel` in 6 of the 11
recordings, 0.8 in 3 and 0.95 in 2, so the default 0.8 stays.

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
| `offload.min_fps_ratio` | 1 | 0–10: a candidate must give at least LOCAL's frames per second times this |
| `offload.p95_slack` | 0.25 | 0–10: at the best's split point, a window within this share above the best value wins with `margin_rel` more frames per second ([Window](#window)); 0 = the lowest value only |
| `offload.max_reject_share` | 0.1 | 0–1: a candidate must not have more than this share of its frames refused by the server |
| `offload.min_hold_ms` | 10000 | 0–3600000: no change before the policy has been in force this long |
| `offload.margin_min_ms` | 2 | 0–1000: the smallest gain (ms) that replaces the current policy (the hysteresis base, step 5 of [`latency`](#latency)) |
| `offload.margin_rel` | 0.1 | 0–1: the gain needed as a share of the current value; the base is the larger of the two |
| `offload.k_queue` | 1 | 0–100; 0 removes the queue term (ablation, phase 3.8) |
| `offload.k_noise` | 1 | 0–100; 0 removes the noise term |
| `offload.queue_rho_from` | 0.8 | 0–0.99: utilization from which the queue term grows; 0 = from no load |
| `offload.reserve` | true | false turns the [reservations](#reservations) off (ablation, phase 3.8) |

`sciot-replay algo -algorithm latency -config config.json` replays a
recording with the parameters of that file's `offload` section.

## Cost

One simulation per `SPLIT` candidate per heartbeat (27 for yolo-small with a
maximum window of 3), and each simulation runs
every other device in `SPLIT` as a client: 0.34 ms per heartbeat alone,
0.73 ms with 1 other, 1.75 ms with 3, 4.5 ms with 7 (MacBook,
`BenchmarkPredictAllClients`). With N devices that is ~N² work per
heartbeat interval.

So decisions run outside the heartbeat handler, in one goroutine of the
server, one device at a time (two devices never decide on the same free
server; the reservations' lock stays for the fallback). The handler queues
the device and answers at once with the current policy. The queue holds
each device once: a new heartbeat of a device already waiting replaces the
older one, so the queue never exceeds the number of devices and every
decision uses the latest state, in the order the devices started waiting.
Between queueing and deciding the device may have been pinned, registered
again or given a policy by the admin: the store refuses such a stale
decision. The `decision` event's `lag_ms` is the time from the heartbeat to
the decision; the replay decides at each heartbeat, without lag.
