# Cost model — what a frame would cost at each split point

`internal/costmodel` predicts the latency of one frame of a device at every
split point, from the server's estimates (see [RECORDING.md](RECORDING.md),
"Estimates"). Phase 3's offloading algorithm uses it to compare split points;
`sciot-replay` checks it against recordings.

    latency(k) = device(0..k) + upload(bytes_k) + wait + S(k)        (SPLIT at k)
    latency    = device(0..last)                                     (LOCAL)

| Term | From |
|---|---|
| `device(0..k)` | the device's time per segment (`segment_ms` in the heartbeats) |
| `upload(bytes_k)` | latency + bytes / bandwidth (`netmeasure`); latency only when the network is not limiting |
| `wait` | queue model below, with the load **after** the move |
| `S(k)` | the dispatcher's service time for split `k` |

A prediction lists its missing inputs (a segment never run, no bandwidth, no
`S`): unknown is not "expensive", and the caller must tell them apart.

## Queue

The dispatcher already predicts the wait of the frame arriving **now**, by
simulating its queue slot by slot: that is admission. The cost model answers
another question: with this device moved to split `k`, what is the **mean**
wait in steady state?

- Load after the move: `ρ' = (Σ λ_i·S_i of the other devices + λ_k·S(k)) / c`,
  with `c` the worker's slots. The device's rate `λ_k` is bounded by the
  slower of its prefix and its upload (a conservative upper bound: the client
  sends without waiting for results); unbounded (split −1 on localhost)
  saturates the server.
- **M/M/c** (Erlang C): `Wq = C(c, a) · S / (c − a)`, `a = λ·S`.
- **G/G/c** (Allen–Cunneen): `Wq ≈ Wq(M/M/c) · (ca² + cs²) / 2`, with `cs` the
  coefficient of variation of `S` (its std / mean) and `ca` that of the
  interval between arrivals (dispatcher `interarrival`).
- `ρ' ≥ 1`: the queue is unstable, the wait unbounded; the real server bounds
  it by refusing frames.

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
  every split point and worker size tried. The algorithm uses it,
  **without** a correction factor fitted on these data: it would depend on
  the machine, the slots and the load.
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
  overestimate, which indeed grows as ρ approaches 1. A queue model with limited capacity
  is the next step only if the replay shows the overestimate leads to wrong
  decisions (verification 2, phase 3.8).
- With one device the queue never forms (regular arrivals, window of 3
  frames): verifying the model needs several devices.

**Limits**: clients and worker on the same machine, so CPU contention adds
noise to `S` (`cs`) and to the device's times, and with many slots it slows
everything down (no data near saturation for `c` = 4). To repeat with the
devices on other machines (phase 4).
