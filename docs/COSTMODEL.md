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

A client without the capability `wait_for_window` never waits for the
server: with its window full (`max_inflight` frames in flight; the algorithm
chooses the window with the split point, [OFFLOAD.md](OFFLOAD.md#window)), or
a frame refused, it finishes the frame itself and goes on. One with it
(the Python client since phase 3.9) waits for a place in a full window when
that costs less than finishing the frame ([CLIENT.md](CLIENT.md#window-full));
a refused frame is still finished at once. So at a split
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
rules of the real code, for 1 s of warm-up and 5 s of measure in virtual time,
or 2 and 30 long device cycles (`A` + prefix + suffix) on a slow device (a
Raspberry Pi sends bursts every ~660 ms: 5 s hold 7 or 8 of them, a 14%
jump), with a fixed seed (the same inputs give the same prediction):

| Who | Rule |
|---|---|
| device, each cycle | `drain`: every error received (refused, expired) is finished on the device, `device(k+1..last)` ms each; then `A` (acquisition and the rest), the frame is born; then the prefix `device(0..k)`; with the window full the device finishes the frame at once, otherwise it sends it **in the background** and starts the next cycle (`ws.send` returns once the bytes are in the kernel's buffer) |
| link | one per device, FIFO: a frame occupies it for `upload − latency` (bytes / bandwidth) after the frames sent before it, and reaches the server one latency later; the deadline counts from there. Shortcut: an unbounded kernel buffer, so `send` never blocks (on a slow link with a large window it does) |
| device with `wait_for_window` | with the window full, before acquiring (after `drain`) it waits `W − A − prefix` and, with the frame ready, up to `device(k+1..last)` for a place, if `W < device(k+1..last)`; `W` = send time of its oldest frame in flight + its estimated round trip (EWMA of the simulated results, α 0.2, as the client) − now. An answer ends the wait at once; errors wait for the next `drain`. Without a round trip measured it does not wait. Recorded per heartbeat (`wait_for_window`), so a replay simulates each client with its own rule |
| answer | returns after `answer_ms − latency` (the device's measured `answer_ms`, phase 4.2: the latency up is already in the frame's arrival), or after the network latency for a device that does not send it; at least 1 ms (measured); it frees the window when it arrives |
| server | admission with `dispatch.PredictWait` (the dispatcher's own function) and the estimated `S`: refused when wait + `S` exceeds the deadline, the device's `device(k+1..last)`; FIFO queue, `c` slots; a frame that gets its slot after its deadline returns as an error |
| other devices | every other device in `SPLIT` is a client with the same rules and its own times at its policy (prefix, suffix, upload, `A`, `S`, window: `costmodel.ClientFrom`); their frames are refused, expire and are drained like the device's, only the device's are counted. Fallback when some other device's inputs are incomplete (or in recordings replayed without them): a stream of frames, always admitted, at the others' measured rate (admitted frames only), with `ca`, `S` and `cs` (log-normal) |

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
waiting for the next frame), handing frames to the socket, handling answers.
Not in the telemetry, the server estimates it from the heartbeat windows
(`device.Cycle`, recorded as `cycle_ms`): the window durations minus the
device's compute (`segment_ms × segment_count`, which includes the frames
finished on the device, unlike `inference_time_ms`), over the frames, as a
time-weighted ratio of sums. The upload is not subtracted: the client sends
in the background. With an image from file it is ~2 ms.

Phase 3.9 found both as errors on a Raspberry Pi: `A` per window (one
heartbeat) swung between −43 and +143 ms (a 636 ms local frame counts whole
in the window where it ends) and, clamped at 0, made cycles too short; and
subtracting the upload made the simulated client block while sending, so its
window never filled.

`Saturated` means the other devices alone already fill the server.

The other devices come from `offload.Fleet`: the last state of every device
(its heartbeat), kept by the server and rebuilt by the replay from the
recorded heartbeats, so both feed the same inputs. A device not seen for 3
heartbeat intervals is dropped; a device pinned by the admin counts like any
other. When the server changes a device's policy, the next decisions
simulate it at the new policy at once. The cost grows with the others:
one decision (all 28 candidates) takes 0.34 ms alone, 0.73 ms with 1 other,
1.75 ms with 3, 4.5 ms with 7 (MacBook, `BenchmarkPredictAllClients`).

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
those frames (a refused one also waits for the next `drain`); the cause and
the fix (the minimum answer time, measured) are at the end of the next
section. Latencies against the client and several devices: next section.

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

That fixed the p95 in all but one configuration (3 devices, window 2:
−25/−26% in four runs, so not noise), but refusals stayed under-predicted
(2.2/s against 6.0 with 2 devices) and frames per second and share
over-predicted by 10–20%: the other devices were a stream of frames
arriving at random and always admitted, while real clients send as soon as
their window frees, keep the queue full and are refused too. The p95 sits
where the slow frames (refused and drained, expired) pass 5%, so it jumps
when their share crosses that line, while the mean is off everywhere
(−8…−14%).

Two prototypes, on the same recordings: the others as a stream at their
*offered* rate that admission can refuse (no change, sometimes worse), and
the others as clients with the device's rules (all errors within ±2%). With
the others as clients (`ClientFrom`, `offload.Fleet`), median error per run:

| Devices, window | p95 | mean | frames/s | share | refused/s predicted / measured |
|---|---|---|---|---|---|
| 1, 2 | +1% | −1% | 0% | 0% | 0 / 0 |
| 2–3, 1 | −1…−2% | −1% | 0% | 0% | 0 / 0 |
| 2, 2 | −1% | −2% | +1% | +1% | 3.4 / 6.0 |
| 3, 2 (4 runs) | −1% | −4…−5% | +1…+5% | +2…+5% | 6.6 / 7–8 |
| 4, 2 | −2% | −4% | +3% | +8% | 7.8 / 8.9 |
| 2, 3 | −1% | −1% | +5% | +4% | 5.0 / 6.0 |
| 3, 3 | −1% | −3% | +4% | +3% | 7.8 / 8.0 |

Not verified yet: devices that differ (model, split point). A real network:
one device, next section.

**Refused or window full, one device at split −1** (window 3, 2 slots):
the simulation gave 14.8 refused frames per second and no full window,
against 7.9 and 7.0 measured; the total finished on the device was right,
and p95, mean and frames per second within 3%. The cause was the minimum
time for an answer to reach the device, 0.1 ms (estimated): a refusal came
back before the device finished its next cycle, so the window was free and
the device sent again. Measured on the client at split −1 (send → answer
minus the server's time): 1.06 ms median, 0.85–1.25 ms between the 10th and
90th percentile; the server alone takes 0.54 ms to read the 1.2 MB frame.
With 1 ms: 7.6 refused and 7.4 window full per second; the runs with several
devices do not change. On a real network the answer time is the client's
`answer_ms` (phase 4.2, below); 1 ms stays the minimum.

That split depended on noise. Since `A` is a smoothed ratio (phase 3.9) it
is a steady 1.10 ms here, just above the 1 ms answer time: every refusal
comes back before the next frame, the window is always free, and the same
recording gives 14.8 refused and no full window (p95, mean, frames/s and
share unchanged: −3%, −2%, +6%, 0%). A deterministic answer time decides a
race the real system runs with a spread (0.85–1.25 ms). Sampling the answer
time, and the device's cycle, moves frames between the two only, never all
the way (phase 4.2, [Variability](#variability-on-the-replay-phase-42)), and
changes no p95, mean or frames/s: not modeled.

### Verification on a Raspberry Pi over Wi-Fi (phase 3.9)

Two Raspberry Pi (`client/results/phase39/`, `docs/RASPBERRY.md`), split −1,
yolo-small: 634 ms per frame on the device (suffix 636 ms), 1.2 MB per frame
in ~85 ms on the link, 2 slots × 2 threads on a MacBook. Runs A1–A3 with the
algorithm (window 2), W1–W3 pinned at window 3, C2 with the camera. Before
the fix the windows 2 and 3 were predicted alike (so the algorithm chose 2),
at twice the measured frames per second and a quarter of the p95: the
simulated client blocked while sending, so its window never filled, and
`A` per heartbeat was mostly clamped to 0. Predicted (median over the
decisions of the steady part) against measured (client CSV, whole steady
part):

| Runs | Window | frames/s before → after / measured | p95 ms before → after / measured | mean ms after / measured |
|---|---|---|---|---|
| A1–A3 | 2 | 9.2–11.6 → 4.44–4.65 / 4.35–4.56 | 154–166 → 635–636 / 652–653 | 333–341 / 380–398 |
| W1–W3 | 3 | 11.4–11.8 → 5.97–6.21 / 5.75–6.04 | 153–172 → 637–638 / 654–656 | 319–328 / 370–372 |
| C2 | 3 | 8.9 → 5.35 / 5.38 | 174 → 647 / 671 | 321 / 387 |

Frames per second within +4%, p95 within −4%, share served exact, frames
with the window full 1.6/s against 1.8. The mean is 12–17% low: the served
frames' latency is 20–29% low, because the simulated link always takes the
same time while the Wi-Fi has a tail (server read time p50 84 ms, p90 175).
It does not move the p95 here (the frames finished on the device set it).
With the served frames setting the p95 (rule c, below) a variable transfer
time did not help either: see [Variability](#variability-on-the-replay-phase-42).
`sciot-replay check` compares per heartbeat (1 s): with bursts of 2–4 frames
every ~660 ms a window holds one or two bursts, so its per-window medians are
biased on such a device (frames/s −13…−17%); use whole runs there. The
phase 3.8 recordings (localhost) do not change: per band within 1–3% of
before, and the bands that were far off before (noisy `A`) are gone.

**With `wait_for_window`** (same Pi, 2026-11-10, 90 s per run). The
algorithm, free, chose split −1 window 1 (predicted p95 151 against 263 for
window 3; `min_fps_ratio` 1 only asks for LOCAL's 1.5 frames/s); one run was
pinned at window 3. No frame was finished on the device. Measured, and
predicted against measured (whole steady part):

| Run | Window | frames/s | p95 ms | frames/s error | p95 error | mean error |
|---|---|---|---|---|---|---|
| A1 | 1 | 5.92 | 273 | +8% | −35% | −9% |
| A2 | 1 | 6.70 | 154 | +10% | −9% | −11% |
| A3 | 1 | 6.75 | 161 | +6% | −6% | −6% |
| C1 (camera) | 1 | 6.91 | 184 | +8% | −25% (−8% without age and acquisition) | −20% (−7%) |
| W1 | 3 | 11.42 | 271 | +1% | −3% | −3% |

Against the strategy-A runs above: 4.4 → 6.7–6.9 frames/s and p95 652 →
161–184 with the algorithm, 5.9 → 11.4 frames/s and p95 655 → 271 at window
3. What the model misses:

- ~10 ms per round trip on the Pi client (`net_ms − recv_ms − 2 × latency`:
  framing and masking 1.2 MB in Python, the receiver thread), where the
  simulation has the 1 ms measured on a MacBook (`simMinAnswerMS`): frames/s
  +6–10% at window 1; at window 3 the link hides it. Since phase 4.2 the
  client measures it (`answer_ms`, next section).
- The camera frame's age (`age_ms`, p50 16 ms, waiting in `LatestFrame`) and
  acquisition: not in the predicted latency. Equal for every policy, LOCAL
  included, so they do not change a choice.
- Wi-Fi stalls (A1: a few frames read in up to 900 ms, none in A3): they set
  that run's p95. Episodic, so a log-normal transfer time would fit them
  badly; not modeled (phase 4.2, next section).
- Found by these runs and fixed: at split −1 with every frame served the
  device runs no segment, and the client did not send `segment_ms`, so the
  server's `A` froze at its earlier value (29.8 ms, or 0). The client now
  always sends them (all `null`/`0`).

### Answer time measured by the client (phase 4.2)

Per frame, `rtt − receive_ms − server_ms` is the latency of the first byte
up, the return of the answer and the client's time to receive it
(`server_ms` includes the queue wait). At window 1, from the client CSVs:

| | p10 | p50 | p90 | latency |
|---|---|---|---|---|
| MacBook, localhost (3.8 P4) | 0.42 ms | 0.52 ms | 0.66 ms | ~0 |
| Raspberry Pi, Wi-Fi (3.9 rule c) | 9.6 ms | 11.4 ms | 14.7 ms | 1.7 ms |

So ~9 ms on the Pi are the Python client's. At window 3 the same difference
is 130 ms (W1_new): the frame also waited on the link behind the previous
one, which the simulation models itself. The client therefore sends
`answer_ms`, the mean over the window of the frames sent with no other frame
in flight (stricter than "link free": the client cannot see bytes still in
the kernel's buffer), absent without such a frame; the server keeps a moving
average (`device.Store`, τ of the segments, recorded in the heartbeat) and
the simulation returns an answer after `answer_ms − latency`, at least 1 ms.
A device at a large window for a long time keeps the value it had.

Wi-Fi stalls (`net_ms` above 3× its median): in 9 runs out of 12, one
episode of 3–6 consecutive frames in 2–6 s, 3–9× the usual time (up to
960 ms), 1–2% of the frames. Clustered, not independent noise per frame:
not modeled. They also reach `answer_ms` (A2_new: p99 48 ms against a p50 of
11); a window at 300 ms moves the average by ~29 ms for ~10 s.

### Variability on the replay (phase 4.2)

Transfer, answer and device cycle are deterministic in the simulation (only
`S` and the fallback's arrivals are sampled, log-normal). Tried on the
replay, each as a log-normal around its value: the rule-c runs of the Pi
with `answer_ms` set from their CSV (W1_new, which has no frame sent
alone, with the mean of A1–A3), predicted (median over the decisions of the
steady part) against measured (whole steady part); and the 3.8 P4 run at
window 3 (localhost), whose refused / window-full split is 7.9 / 7.0
measured:

| Variant | p95 error A1 / A2 / A3 / C1 / W1 | P4: refused / window full |
|---|---|---|
| none (`answer_ms` only) | −32 / −3 / −1 / −20 / −3% | 14.8 / 0 |
| transfer, CV of the bandwidth (median 0.06) | −3 / +2 / +6 / −17 / +8% | 14.8 / 0 |
| transfer, CV 0.45 (of `receive_ms`) | +11 / +59 / +52 / +32 / +41% | 14.8 / 0 |
| answer, CV 0.1–0.4 | within ±3% of none | ~12.7 / 2.4 |
| cycle CV 0.5 + answer CV 0.2 | — | 9.6 / 5.2 |

Without variability, frames/s are within −1…+2% and the mean within −5% on
the Pi (C1: −15%, the camera frame's age), and the p95 within −3% except A1
(Wi-Fi stalls) and C1. The bandwidth's CV fixes A1 only because the stall
is in the bandwidth's standard deviation (stalls are not modeled, above),
and costs +5…+8% on the clean runs; 0.45 is far too much. Answer and cycle
spreads move P4's frames between refused and window full (both finished on
the device), never all the way, with p95, mean and frames/s unchanged
(−2%, −1%, +6% in every variant). So none is modeled; to look at again with
a slow network and several devices on other machines.

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
| `A` | `Inputs.CycleMS` | ms | the rest of a device cycle (acquisition, handing frames to the socket, answers) | `device.Cycle` (heartbeat `cycle_ms`), telemetry |
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
