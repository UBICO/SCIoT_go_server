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
`answer_ms`, the mean over the window of the frames that travelled alone:
no other frame in flight when sent (stricter than "link free": the client
cannot see bytes still in the kernel's buffer) and none sent before the
answer, which the client's own send of the next frame delays (campaign R2,
window 2: one such sample, 66 ms, kept for the whole run and frames/s
predicted −7%); absent without such a frame; the server keeps a moving
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
| transfer, CV of the bandwidth (median 0.06, of the rates) | −3 / +2 / +6 / −17 / +8% | 14.8 / 0 |
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
a slow network. With two devices on other machines and a small model the
transfer's spread matters ([below](#verification-on-the-pi-with-answer_ms-phase-42-campaign),
two models), still not modeled.

### Verification on the Pi with `answer_ms` (phase 4.2 campaign)

One Raspberry Pi (`pi1`, static image, split −1, yolo-small, Wi-Fi, server
on a MacBook; [RASPBERRY.md](RASPBERRY.md#8-phase-42-campaign),
`client/results/phase42/`), 2026-11-10. R1: the algorithm free, 4 runs
(R1_4 after the fix of the window hysteresis); R2: windows 1, 2, 3 pinned.
Predicted: median over the decisions at that window (R1, live; R2, replay,
since a pinned device gets no decisions); measured: client CSV, every frame
at that window.

| Runs | Window | frames/s measured / predicted | p95 ms measured / predicted |
|---|---|---|---|
| R1_1–R1_4 | 1 | 7.04–7.13 / 6.81–7.19 | 151–158 / 148–164 |
| R1_1–R1_4 | 2 | 11.60–11.64 / 11.60–11.78 | 182–185 / 176–179 |
| R2_w1 | 1 | 6.78 / 6.83 | 155 / 151 |
| R2_w2 | 2 | 11.23 / 11.68 | 221 (Wi-Fi tail at 40–68 s; ~185 without) / 175 |
| R2_w3 | 3 | 11.42 / 11.49 | 278 / 265 |

Frames/s within −3…+2% and p95 within −2…−5% (with the R2 replay's
`answer_ms` set to the 11.7 ms measured at window 1: the run's own value
came from a single frame whose answer the next send delayed, fixed since,
[above](#answer-time-measured-by-the-client-phase-42)). `answer_ms` 10–13 ms
in every run. The window 2 is the knee: +65% frames/s for +20% p95 against
window 1, while window 3 adds 2% frames/s for +50% p95 (the link is full).

**Server time profiled against measured.** With the client on the Pi the
Mac runs only server and worker: `S` at split −1 profiled at start (5 runs)
38.5–45.4 ms, measured in the same runs (`server_ms − queue_wait_ms`,
median) 40.0–47.4 ms, −4…+14%. In phase 3.8, with three clients loading
TensorFlow on the server's machine, it was 42 against 28 (+50%). With one
device on another machine the CPU contention is gone; with two (R3–R4,
below) too: profiled 37.0–38.9 ms, measured 36.5–38.5.

**Two devices** (R3 ×3, R4 ×4: `pi1` and `pi2`, both yolo-small, started
together, 2026-11-17). Every run, every device: `LOCAL` → split −1 window 1
(`latency`, 22–25 s) → window 2 (`fps`, 10 s later) and no other change; in
R4_kqueue0_2 `pi2` moved to window 2 after 15 s instead of 10 (predicted p95
ratio 1.25–1.32, on the edge of the slack). Load ρ 0.3–0.4, queue wait 0.
Predicted (median over the decisions after the last change) against
measured (client CSV, from 5 s after it):

| | frames/s | mean | p95 |
|---|---|---|---|
| predicted / measured | 9.8–10.7 / 10.0–10.7 | −0…−8% | 194–216 / 221–249 ms (−12…−20%) |

Frames/s and mean as with one device; the p95 is 12–20% low, against −3%
for `pi1` alone (R1): the Wi-Fi shared by the two devices, a longer
transfer tail that the simulation does not have (checked in
[phase 4.4](#slow-network-shared-wi-fi-and-pico-alone-phase-44)).
With `k_noise: 0` or `k_queue: 0` (R4) the decisions and the measures are
those of R3: with ρ 0.4 and no queue neither term is active
([OFFLOAD.md](OFFLOAD.md#verification-phase-38)). The queue term
(`k_queue`) was removed in phase 4.3.

**Two models** (R5 ×2: `pi1` yolo-small, `pi2` yolo-pico 320, 2.3 MB,
~200 KB per frame at split 4). `pi1` as in R3 (window 2, 10.6 frames/s, p95
221). `pi2`: split 4 window 1 (`latency`) → window 3 (`fps`): the simulation
gives the three windows the same p95 (the device bounds the rate), so the
largest wins. Measured at window 3 53 frames/s, mean 61–63 ms, p95 79–80,
against a predicted 65, 37–41, 37–43 (frames/s +22…+24%, mean −35…−40%, p95
−47…−53%); measured at window 1 (its 10 s) p95 51–60, so window 3 breaks
the slack (×1.55 > 1.25).

The main cause is the bandwidth estimate, not the client: `netmeasure`
averaged each sample's bandwidth (bytes/s, until the change below), and the
transfer time is bytes divided by that average. For small frames with
spread read times (pico: a few 2–4 ms samples at 50–100 MB/s among ones
of ~18 ms, CV of the time per byte 0.46) the mean of the rates is far above bytes / mean time: 15.6–16.7
against 11.0–11.4 MB/s, so 12 ms predicted per frame against 16–17
measured. For yolo-small (1.2 MB, CV 0.03–0.15) the two are equal. The
answer measured at window 3 (28 ms, against 11–14 at window 1) is mostly
this transfer queueing on the link behind the previous frame. Replay of
every 3.9 and 4.2 run (27 run/device pairs) with the bandwidth recomputed
from the recorded samples as the average of the time per byte:

| | frames/s | p95 | mean |
|---|---|---|---|
| yolo-small, median \|error\| (25 pairs), now → time per byte | 2.4 → 2.8% | 12.0 → 10.8% | 3.7 → 1.8% |
| R5 `pi2`, now | +22…+24% | −47…−53% | −35…−40% |
| R5 `pi2`, time per byte | +6…+10% | −18…−19% | 0…+4% |

yolo-small's decisions do not change (1 of 158 in R4_kqueue0_2); R5_1's
`pi2` picks window 2 (45 of 70 decisions), R5_2's stays at window 3. The
one regression is A1 (phase 3.9, Wi-Fi stalls): frames/s −13%, mean +10%,
same decision. The 3.8 recordings (localhost) have no bandwidth sample.
Pico at window 2 was not measured here ([phase 4.3](#loaded-server-and-pico-on-the-pi-phase-43-campaign)). The p95 left out (−18%) is the
transfer's spread: a per-device CV of the time per byte brings yolo-small's
p95 error to 4% but A1's to +75…+127%, and sends R5_2's `pi2` to `LOCAL`
(46 of 69 decisions), which is measured worse (~14 frames/s, p95 66,
against 26–29 frames/s, p95 51–60 at split 4 window 1): only SPLIT would get
a spread, `LOCAL` none. Not modeled.

Since phase 4.2 `netmeasure` averages the time per byte
([RECORDING.md](RECORDING.md#network)): the "time per byte" rows above are
what the server now estimates (the same code, rerun on the recorded
samples). `bandwidth_std_bps` keeps the time per byte's CV, so the CV of
the bandwidth [above](#variability-on-the-replay-phase-42) was
computed on the rates, before the change.

### Loaded server and pico on the Pi (phase 4.3 campaign)

Two Raspberry Pis over Wi-Fi, static image, server on a MacBook at commit
`59dd88d` ([RASPBERRY.md](RASPBERRY.md#9-phase-43-campaign),
`client/results/phase43/`), 2026-11-17, 90 s per run. Predicted: median
over the decisions after the last policy change (live; replay for pinned
devices); measured: client CSV from 5 s after it. Utilization: the
dispatcher's, median over the steady part.

**Loaded server.** The worker with **1 slot** brings the two Pis' load near
saturation, which two devices on 2 slots never reached (ρ 0.3–0.4 in phase
4.2): `S` at split −1 is 36 ms with 2 threads, 67 ms with 1. Both Pis
yolo-small.

| Run | Worker | Policies | Utilization | frames/s measured (error) | p95 ms measured (error) |
|---|---|---|---|---|---|
| Q1 ×2, free | 1 × 2 | −1/2, −1/2 | 0.71–0.73 | 10.3–10.4 (−5…0%) | 227–243 (−10…+6%) |
| Q2 ×2, free, `k_queue: 0` | 1 × 2 | −1/2, −1/1 | 0.59 | 11.1 / 5.7 (−3…+4%) | 210–222 / 198 (−19…−9%) |
| Q3, pinned −1/3 | 1 × 2 | −1/3, −1/3 | 0.72 | 10.3–10.4 (−5…−1%) | 342–346 (−6…+2%) |
| Q4, pinned −1/2 | 1 × 1 | −1/2, −1/2 | 0.94 (p90 0.99) | 7.27 (0%) | 288–292 (−2…−1%) |
| Q5, free | 1 × 1 | 4/2, −1/2 | 0.95 | 7.9 (0%) | 361 / 258 (0…+1%) |
| Q5k, free, `k_queue: 0` | 1 × 1 | −1/2, −1/1 | 0.92 | 9.3 / 4.6 (+3…+4%) | 251 / 273 (−16…−8%) |

The simulation holds near saturation: in Q4 and Q5 (utilization 0.94–0.95,
queue wait 79–113 ms) frames/s and mean within 3%, p95 within 2%. The
error left is the p95 of two devices on different windows (Q2, Q5k: −8…−19%,
as in [phase 4.2](#verification-on-the-pi-with-answer_ms-phase-42-campaign)).
Q3 gets the frames/s of window 2 at a higher p95: at window 3 the link,
not the server, bounds the rate. No frame refused by the server in any run
(4 `deadline_exceeded` in 29 826 frames of the whole campaign). The queue
term of the algorithm's margin froze Q5 near saturation and was removed
([OFFLOAD.md](OFFLOAD.md#verification-phase-38)).

**Queue formulas** (`sciot-replay queue`, the pending repetition of
[verification 1](#verification-1-2026-10-28) with devices on other
machines): measured mean wait per band against M/M/c and G/G/c.

| Run | ρ band (windows) | measured | M/M/c | G/G/c |
|---|---|---|---|---|
| Q3 | 0.5–0.8 (15) | 8.6 ms | 123 ms | 23 ms |
| Q4 | 0.95–1 (8) | 84.7 ms | 42 437 ms | 2 944 ms |
| Q4 | ≥ 1 (7) | 83.4 ms | ∞ | ∞ |

Near saturation both formulas are wrong by orders of magnitude: they
describe an open queue, while here each device has at most its window in
flight, so the queue is bounded by the sum of the windows (Q4: at most 4
frames in flight, measured wait ~1.3 `S`). The simulation models the windows; the formulas stay
superseded.

**Pico after the bandwidth from the time per byte.** `pi1` yolo-small free,
`pi2` yolo-pico 320 free (P1 ×2) or pinned at split 4 (P2).

| `pi2` | frames/s measured / predicted | p95 ms measured / predicted | mean ms measured / predicted |
|---|---|---|---|
| P2, 4/1 | 27.0 / 31.6 (+17%) | 52 / 45 (−13%) | 44 / 42 (−5%) |
| P2, 4/2 | 44.1 / 61.2 (+39%) | 62 / 44 (−29%) | 52 / 43 (−17%) |
| P2, 4/3 | 56.0 / 60.5 (+8%) | 74 / 61 (−18%) | 59 / 60 (+2%) |
| P1_1, free → 4/2 | 42.5 (+35%) | 68 (−31%) | 53 (−16%) |
| P1_2, free → 5/2 | 41.5 (+43%) | 69 (−33%) | 55 (−19%) |

The choice is now right: measured, window 2's p95 is 1.19× window 1's
(within `p95_slack`) and window 3's 1.42× (outside), and both free runs end
at window 2 (split 4 or 5, predicted equal). In R5 (before the change) it
was window 3. The error is not: the simulation gives window 2 the frames/s
of a full pipeline (61, as window 3), measured 44 against 56: the other
Pi's traffic and an answer that grows with the frames in flight, not a CPU
cost on the client ([phase 4.4](#slow-network-shared-wi-fi-and-pico-alone-phase-44)).
`pi1` (yolo-small, ρ 0.3) frames/s −2…+3%, p95 −8…−14%.

### Slow network, shared Wi-Fi and pico alone (phase 4.4)

The three checks left from phase 4: a slow network on the Pi, the p95
12–20% low with two devices, pico at window 2. The last two first offline,
on the 4.2 and 4.3 recordings; then one Raspberry Pi at a time (`pi2`),
static image, server on a MacBook at commit `28cc069`
([RASPBERRY.md](RASPBERRY.md#10-phase-44-campaign), `client/results/phase44/`),
2026-11-18. Predicted: live decisions after the last change (free runs),
replay (pinned runs); measured: client CSV.

**Slow network.** Upload capped on the Pi with `tc` (token bucket),
yolo-small, the algorithm free (R6a) or pinned at split −1 window 1 (R6b):

| Cap | Policy | frames/s measured (error) | p95 ms measured (error) |
|---|---|---|---|
| 40 Mbit/s | −1/1 after 37 s | 3.15 (0%) | 331 (0%) |
| 20 Mbit/s | 4/1 after 26 s | 2.57 (+2%) | 514 (−4%) |
| 10, 5 Mbit/s | `LOCAL` | 1.54 (−1%) | 640 (−1%) |
| 10 Mbit/s, pinned | −1/1 | 1.90 (+1%) | 1050 (0%) |

Mean within 1%, one policy change per run and none back. At 10 Mbit/s
split −1 was predicted at p95 966 against `LOCAL`'s 634 (1.52×, beyond
`p95_slack`), measured pinned at 1050 against 640: staying `LOCAL` was
right. In the pinned run half the frames finish on the device (window
full: waiting would cost more than the 634 ms local frame), and the
simulation has them too. The bandwidth estimate with frames flowing is
within 1–3% of the cap (39.9, 20.6, 10.1 Mbit/s); with only probes (the
`LOCAL` runs) +8% (10.8, 5.4): after a pause the token bucket lets 64 KiB
through at once, a large share of a 512 KiB probe. The latency stays at
1.5–3.4 ms in every R6 run, the pinned one too (link 94% busy since the
start): only clean pings count ([RECORDING.md](RECORDING.md#network), fixed
before this campaign; with the old rule `bench_network.py` gave 1968 ms
after 60 s at 5 Mbit/s). Pico pinned at window 3 (PA_w3, below, link ~95%
busy) had 4–7 ms against 1.5 at windows 1 and 2, from pings during which no
message finished, so not the upload queue (perhaps the busy client answering
pings late); its prediction is right all the same.

**Two devices: the shared Wi-Fi.** The low p95 with two Pis (phase 4.2 R3–R4,
phase 4.3 Q) comes from the transfer. Same Pi, model and split (`pi1`,
yolo-small, −1), `receive_ms` against the other Pi's traffic:

| `pi2` sends | `pi1` `receive_ms` p50 / p95 | CV |
|---|---|---|
| nothing (R1) | 86 / 91 ms | 0.04 |
| 5.7 MB/s (P2, pico window 1) | 88 / 99 ms | 0.07 |
| 8–10 MB/s (P1, P2, R5) | 89–92 / 103–114 ms | 0.09–0.12 |
| 12 MB/s (R3, R4, Q: yolo-small) | 95–97 / 115–126 ms | 0.14–0.17 |

Within one run too (P1_1: `pi1`'s frames whose transfer `pi2`'s did not
overlap 84 / 90 ms, overlapped 91 / 105), so it is not the day (R1 ran a
week earlier). The frames above the p95 lose their time on the link: the
transfer, the wait behind the previous frame, the wait for a full window;
queue wait 0 and `S` +0…4 ms. The mean is right because the bandwidth (time
per byte) already includes the sharing: only the spread is missing, the
per-device transfer spread of [phase 4.2](#verification-on-the-pi-with-answer_ms-phase-42-campaign),
not modeled.

**Pico at window 2.** On the client neither the prefix nor `A` grows with
the window (P2: prefix 10.2 / 10.3 / 10.4 ms, `A` 4.9–5.3 at windows 1, 2,
3), and answers are stamped also while a prefix runs (the receiver thread
is not held by the interpreter): no CPU cost on the client. The lost cycle
is the wait for a full window, 20.5 / 5.9 / 1.9 ms a frame: at window 2 the
window bounds the rate, while the simulation has the device bound it. The
window's place is held longer than simulated, for two reasons. The other
Pi: alone (PA runs, `pi1` idle) pico's transfer CV is 0.12–0.18 against
0.34–0.45 and its answer at window 1 6 ms against 14, and window 2 gives
53.6 frames/s against 45.9 with `pi1` running (same steady part). And the
answer grows with the frames in flight even alone (`rtt − receive − server
time` 6.0 / 11.0 / 16.8 ms at windows 1, 2, 3), while `answer_ms` comes
from frames sent alone, at window 1: the replay of PA_w2 with 11 ms gives
+8% frames/s and +1% p95. Whether the Pi's own upload delays the answer
on the half-duplex Wi-Fi or the client does, these runs cannot tell (no
Pi on a cable). The choice is unaffected: measured and predicted, window 2.
`S` of pico on the Mac was 7–13 ms that day against 4.3 in phase 4.3,
measured and followed by the model.

| PA, `pi2` pico 4/w | frames/s measured / predicted | p95 ms measured / predicted |
|---|---|---|
| window 1 | 27.6 / 30.1 (+9%) | 47 / 46 (−1%) |
| window 2 | 53.6 / 65.0 (+21%) | 48 / 40 (−17%) |
| window 3 | 63.6 / 63.8 (0%) | 59 / 60 (+2%) |

Not modeled, both wait for the ESP32 client (other costs than the Python
one): a per-device transfer spread, and an answer measured with frames in
flight, which needs the server to mark the frames that found the link free
(the simulation already queues the others on the link).

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
everything down (no data near saturation for `c` = 4). Repeated in phase
4.3 with two Raspberry Pis and 1 slot, up to ρ ≥ 1
([above](#loaded-server-and-pico-on-the-pi-phase-43-campaign)): both
formulas fail near saturation, because the windows bound the queue.
