# Cost model — what a frame would cost at each split point

`internal/costmodel` predicts the latency of one frame of a device at every
split point, from the server's estimates (see [RECORDING.md](RECORDING.md),
"Estimates"). Phase 3's offloading algorithm uses it to compare split points;
`sciot-replay` checks it against recordings.

    server(k) = device(0..k) + upload(bytes_k) + wait + S(k) + return    a frame the server serves
    local(k)  = device(0..k) + device(k+1..last)                a frame the device finishes
    latency(k) = share·server(k) + (1 − share)·local(k)          (SPLIT at k)
    latency    = device(0..last)                                 (LOCAL)

| Term | From |
|---|---|
| `device(0..k)` | the device's time per segment (`segment_ms` in the heartbeats) |
| `upload(bytes_k)` | latency + bytes / bandwidth (`netmeasure`); latency only when the network is not limiting |
| `wait` | queue wait with the device moved to `k`, from the simulation below |
| `return` | the result's way back: the network latency |
| `S(k)` | the dispatcher's service time for split `k` |
| `share` | the share of the device's frames the server serves, below |

A prediction lists its missing inputs (a segment never run, no bandwidth, no
`S`): unknown is not "expensive", and the caller must tell them apart.

## Frames the device sends: simulation

The client never waits for the server: with its window full (`max_inflight`
frames in flight; the algorithm chooses the window with the split point,
[OFFLOAD.md](OFFLOAD.md#window)), or a frame refused, it finishes the frame
itself and goes on. So at a split
point only part of the frames may reach the server (`share`). Value-average
formulas got this badly wrong (3× too many frames to the server at split −1),
because two behaviours depend on the exact order of events:

- **a device's frames queue behind each other**: with a window of 3 and 2
  slots, the third frame in flight finds both slots taken by the same
  device's frames; predicted wait + `S` exceeds its deadline and the server
  refuses it, every time;
- **the device stops sending while it finishes a frame**: the local
  completion runs on the client's main loop (`drain`), so for
  `device(k+1..last)` ms nothing is sent and the slots sit idle.

For a SPLIT point `Predict` therefore runs a discrete-event simulation
(`internal/costmodel/sim.go`) of the device and the dispatcher, with the
rules of the real code, for 1 s of warm-up and 5 s of measure in virtual time
(fixed seed: the same inputs give the same prediction):

| Who | Rule |
|---|---|
| device, each cycle | `drain`: every error received (refused, expired) is finished on the device, `device(k+1..last)` ms each; then `A` (acquisition and the rest), the frame is born; then the prefix `device(0..k)`; with the window full the device finishes the frame at once, otherwise it sends it (`upload` ms) |
| answer | returns after the network latency (at least 0.1 ms) and frees the window when it arrives |
| server | admission with `dispatch.PredictWait` (the dispatcher's own function) and the estimated `S`: refused when wait + `S` exceeds the deadline, the device's `device(k+1..last)`; FIFO queue, `c` slots; a frame that gets its slot after its deadline returns as an error |
| other devices | a stream of frames, always admitted (their measured rate counts only admitted frames), with their rate, `ca`, `S` and `cs` (log-normal) |

Results per split point: frames per second of the device and served by the
server, `share`, refused and window-full frames per second, mean queue wait
of the served frames, mean latency of a served frame (prefix to answer), of
one finished on the device (for a refused one, including the wait for the
next `drain`), overall mean and **p95**; `ρ` from the simulated slot busy
time. `Bound` is `device` when no frame is finished on the device, otherwise
the most frequent cause (`window` or `deadline`). LOCAL stays a formula
(exact): `device(0..last)`, at `1000 / (A + device(0..last))` frames per
second.

`A` is the rest of a device cycle: reading the image (with a camera, also
waiting for the next frame), handling answers. Not in the telemetry, it is
derived from each heartbeat window (`offload.CycleMS`): window duration minus
the device's compute (`segment_ms × segment_count`, which includes the
frames finished on the device, unlike `inference_time_ms`) minus the upload
of the frames sent, per frame. With an image from file it is ~2 ms.

`Saturated` means the other devices alone already fill the server.

**Coupling**: the simulation mirrors the client. A change of the client's
window or fallback behaviour must change `sim.go` too.

### Verification (2026-10-30)

`sciot-replay algo` on one device, 40 s per split point, yolo-small, 2 slots
× 2 threads, client and worker on the same MacBook. Predicted (mean over the
heartbeats) against measured (server records and device telemetry):

| Split | Device fps | Served by the server /s | Share | Refused /s |
|---|---|---|---|---|
| 6 | 52.3 / 51.9 | 52.3 / 51.9 | 1.00 / 1.00 | 0 / 0 |
| 3 | 41.9 / 41.2 | 31.5 / 30.7 | 0.75 / 0.75 | 0 / 0.1 |
| −1 | 30.6 / 30.3 | 15.3 / 15.2 | 0.50 / 0.50 | 14.4 / 7.6 |

Frames per second and share are within 3%. At split −1 the frames finished
on the device are right in total (15.3 against 15.1) but split differently
between their causes: half of the real ones found the window full, almost
none in the simulation. The order of a refusal and the next cycle decides
which; it does not change share or rates, but it changes the latency of
those frames (a refused one also waits for the next `drain`). Latencies
against the client and several devices: next section.

### Verification with the client, several devices (phase 3.8)

`sciot-replay check record.jsonl frames_<device>.csv...` compares, at every
heartbeat of a device in SPLIT, the prediction for the pair it applies with
what the client measured in the same window (its frames CSV, aligned on
`captured_at`): p95, mean, frames/s, share served, frames refused or expired
and frames that found the window full. The server does not see frames the
device finishes; the client does. Errors are (predicted − measured) /
measured, median per band of predicted `ρ`.

Recordings with `client/scripts/bench_multi.py --pin 6/w`: 1 to 4 devices
held at split 6, window 1–3, 2 slots × 2 threads, 120 s, clients and worker
on the same MacBook (2026-11-05). The other devices' load was first taken as
the total minus the device's frames per second, frames never admitted
included (refused or window full), so with several devices in SPLIT it came
out 30–60% low (4 devices: 26.6 frames/s against 39.2). Median error of the
p95, before and after taking the device's admitted load on the same window:

| Devices, window | before | after | refused/s predicted / measured, after |
|---|---|---|---|
| 1, 2 | +4% | +1% | 0 / 0 |
| 2, 1 | −1% | −1% | 0 / 0 |
| 2, 2 | −22% | −2% | 2.2 / 6.0 |
| 3, 2 | −26% | −26% | 4.0 / 7.0 |
| 4, 2 | −27% | −2% | 5.8 / 8.9 |
| 2, 3 | −1% | −1% | 5.4 / 6.0 |
| 3, 3 | −26% | −1% | 7.0 / 8.0 |

The p95 is now within 3% in all but one run (3 devices, window 2: one run
per configuration, the spread between runs is not measured yet). Refusals
are still under-predicted with several devices, and with them the frames
per second and the share served (+10–20%): the simulation treats the other
devices as a stream that is always admitted and does not react, while they
are clients like this one, with their own window and refusals.

## Mean or p95

Every SPLIT prediction has both the mean latency of the device's frames
(`total_ms`) and their 95th percentile (`p95_ms`). They describe two
different experiences of the same system:

- **mean**: what a typical frame costs, the work done on average;
- **p95**: how slow the slowest 5% are, how regular the latency is.

They choose differently when only part of the frames reaches the server:
the mean may prefer a split point where some frames are finished on the
device (fast and slow frames averaging out), the p95 prefers split points
where every frame takes the same path. From the recordings of the
verification above:

| Split | Mean | p95 | Why |
|---|---|---|---|
| 6 | 43.6 ms | 44.6 ms | every frame served by the server: latencies all alike |
| 3 | 50.0 ms | 65.5 ms | 25% of the frames finished on the device |
| −1 | 65.9 ms | 124.7 ms | half served (37 ms), half refused and finished after the next `drain` (94 ms) |

The offloading algorithm (phase 3.6) compares one of the two
(`offload.objective`), and both are kept, for three reasons:

1. **Experiments**: replaying the same recording with both shows how much
   the choice matters (how many decisions change, by how much), instead of
   assuming it.
2. **Applications**: real-time control (a camera driving an action) cares
   about the slow frames, p95; analysis that only counts frames processed
   can use the mean.
3. **Reactivity**: the mean comes from all the simulated frames, so it is
   less noisy than the p95 (from ~150–260 frames per split point); with a
   smaller noise the algorithm's margin is smaller and it reacts sooner. The
   p95 is steadier but slower.

The default is p95: the goal is a stable latency.

## Queue formulas (verification 1, superseded)

These closed-form formulas were the cost model's queue until phase 3.5c; the
simulation above replaced them in `Predict`. They remain in
`internal/costmodel/queue.go` for `sciot-replay queue` and the verification
below.

The dispatcher already predicts the wait of the frame arriving **now**, by
simulating its queue slot by slot: that is admission. The cost model answers
another question: with this device moved to split `k`, what is the **mean**
wait in steady state?

- Load after the move: `ρ' = (Σ λ_i·S_i of the other devices + λ_k·S(k)) / c`,
  with `c` the worker's slots and `λ_k` the device's frames per second at
  `k`.
- **M/M/c** (Erlang C): `Wq = C(c, a) · S / (c − a)`, `a = λ·S`.
- **G/G/c** (Allen–Cunneen): `Wq ≈ Wq(M/M/c) · (ca² + cs²) / 2`, with `cs` the
  coefficient of variation of `S` (its std / mean) and `ca` that of the
  interval between arrivals (dispatcher `interarrival`).
- `ρ' ≥ 1`: the queue is unstable, the wait unbounded; the real server bounds
  it by refusing frames.

## Variables

| Symbol | In the code | Unit | Meaning | Source |
|---|---|---|---|---|
| `c` | `Queue.Slots` | — | servers: frames the worker runs in parallel | worker slots |
| `λ` | `lambda` | frames/s | arrival rate at the server | devices' rates; replay: served frames / window |
| `S` | `ServiceMS`, `s` | ms (s in the formulas) | mean service time, queue excluded | dispatcher estimate per split point |
| `a` | `a` | erlang | offered load `λ·S`: slots busy on average | computed |
| `ρ` | `Rho` | — | utilization per slot `a / c`; ≥ 1 = unstable queue | computed |
| `ca` | `Queue.CA` | — | std / mean of the intervals between arrivals: 0 regular, 1 Poisson | dispatcher `interarrival` |
| `cs` | `Queue.CS` | — | std / mean of `S`: 0 constant, 1 exponential | std of the `S` estimates |
| `C(c, a)` | `ErlangC` | probability | an arriving frame has to wait (all slots busy), M/M/c | Erlang B recursion `B(k)` |
| `Wq` | `WaitMMc`, `WaitGGc` | s (ms in `Prediction`) | mean wait in queue, M/M/c or G/G/c | formulas above |
| `λ_others`, `a_others` | `Queue.OthersLambda`, `Queue.OthersErlangs` | frames/s, erlang | load of the other devices, without the one moved | dispatcher total minus the device's own load on the same window (`offload.Others`; recording: `device_admitted_per_s`, `device_erlangs`) |
| `λs` | `Prediction.ServerFramesPerSec` | frames/s | the device's frames the server serves at split `k` | simulation |
| — | `Prediction.FramesPerSec`, `ServerShare` | frames/s, — | all the device's frames per second, and the share served | simulation |
| `ρ'` | `Prediction.Rho` | — | utilization with the device at `k`: simulated slot busy time / (`c` × time) | simulation |
| `A` | `Inputs.CycleMS` | ms | the rest of a device cycle (acquisition, answers) | `offload.CycleMS`, telemetry |
| — | `Inputs.MaxInflight` | frames | the device's window | stream, heartbeat |
| — | `Prediction.Saturated` | — | the other devices alone fill the server (`a_others ≥ c`): none of this device's frames would be served | computed |
| — | `Prediction.Missing` | — | unknown inputs (segment, network, `S`, slots) | computed |

In `sciot-replay queue`, per window: arrival = `t_ms − timing.server_ms`,
service = `server_ms − queue_wait_ms`, measured wait = mean `queue_wait_ms`,
`c` = `queue.slots` of the last heartbeat; `λ`, `S`, `ca`, `cs` from the
served frames of the window. Bands of `ρ`: 0–0.5, 0.5–0.8, 0.8–0.95,
0.95–1, ≥ 1.

## Verification 1 (2026-10-28)

`go run ./cmd/sciot-replay queue record.jsonl`: per 5 s window, the measured
mean queue wait against the two predictions, with `λ`, `S`, `c`, `ca` and `cs`
measured in the same window (arrival = `t_ms − timing.server_ms`, service =
`server_ms − queue_wait_ms`, frames served by the server only).

yolo-small, MacBook; one, then two, then three devices added every 40 s, all
on the same machine as the server. Mean wait per utilization band (windows
in brackets):

| Split, worker | S, ca, cs | ρ | measured | M/M/c | G/G/c |
|---|---|---|---|---|---|
| 6, 2 slots × 2 threads | 27 ms, 0.6, 0.03 | 0–0.5 (1) | 0.00 ms | 1.79 ms | 0.00 ms |
| | | 0.5–0.8 (9) | 1.06 ms | 21.68 ms | 1.47 ms |
| | | 0.8–0.95 (16) | 9.95 ms | 82.81 ms | 19.67 ms |
| 4, 2 slots × 2 threads | 31 ms, 1.1, 0.02 | 0–0.5 (1) | 6.18 ms | 1.99 ms | 1.13 ms |
| | | 0.5–0.8 (23) | 9.45 ms | 36.52 ms | 21.04 ms |
| | | 0.8–0.95 (2) | 11.74 ms | 63.37 ms | 32.69 ms |
| 6, 4 slots × 1 thread | 50 ms, 0.3, 0.01 | 0–0.5 (4) | 0.00 ms | 0.82 ms | 0.01 ms |
| | | 0.5–0.8 (2) | 0.00 ms | 6.93 ms | 1.15 ms |
| 6, 1 slot × 4 threads | 16 ms, 0.4, 0.04 | 0–0.5 (1) | 16.74 ms | 14.66 ms | 2.31 ms |
| | | 0.5–0.8 (8) | 0.00 ms | 47.64 ms | 0.06 ms |
| | | 0.8–0.95 (7) | 16.88 ms | 187.65 ms | 40.43 ms |
| | | 0.95–1 (10) | 19.97 ms | 481.89 ms | 74.47 ms |

`ca` and `cs` are the mean over the windows.

- **M/M/c overestimates 4–25×** in every configuration: service is almost
  constant (`cs` ≤ 0.04) and arrivals are not Poisson. Kept only as an
  upper bound.
- **G/G/c is close below ρ ≈ 0.8 and 2–4× above it**, the more the closer to
  1 (1 slot: 2.4× at 0.8–0.95, 3.7× at 0.95–1), on the prudent side, for
  every split point and worker size tried. It was the cost model's queue
  until the simulation replaced it (3.5c), **without** a correction factor
  fitted on these data: it would depend on the machine, the slots and the
  load.
- The few single windows where the measured wait is higher than both
  predictions are at the start of a stage (a device just arrived, queue
  not yet in steady state): a steady-state model does not describe them.
- With 4 slots the three devices never pushed ρ above 0.8 (S grew to 50 ms
  with four inferences and three clients on the same CPU): no data near
  saturation for `c` = 4.
- **Admission cuts the queue**: near saturation the dispatcher refuses the
  frames that would not finish in time (split 6 with 2 slots: ρ stays below
  ~0.9 with the third device). The real queue is shorter than the
  unconstrained one the formula describes: the likely main cause of the
  overestimate, which indeed grows as ρ approaches 1. The simulation
  (3.5c) applies the admission rule itself, so it does not have this bias.
- With one device the queue never forms (regular arrivals, window of 3
  frames): verifying the model needs several devices.

**Limits**: clients and worker on the same machine, so CPU contention adds
noise to `S` (`cs`) and to the device's times, and with many slots it slows
everything down (no data near saturation for `c` = 4). To repeat with the
devices on other machines (phase 4).
