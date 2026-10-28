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

yolo-small, split 6 (S ≈ 27 ms), worker 2 slots × 2 threads, MacBook; one,
then two, then three devices added every 40 s (all on the same machine):

| ρ | windows | measured | M/M/c | G/G/c |
|---|---|---|---|---|
| 0–0.5 | 1 | 0.00 ms | 1.79 ms | 0.00 ms |
| 0.5–0.8 | 9 | 1.06 ms | 21.68 ms | 1.47 ms |
| 0.8–0.95 | 16 | 9.95 ms | 82.81 ms | 19.67 ms |

- **M/M/c overestimates 8–20×**: service is almost constant (`cs` ≈ 0.03) and
  arrivals are not Poisson (`ca` ≈ 0.6 with several devices, ≈ 0.05 with one).
  Kept only as an upper bound.
- **G/G/c is close at medium load and ~2× above near saturation**, on the
  prudent side. The algorithm uses it, **without** a correction factor fitted
  on these data: it would depend on the machine, the slots and the load.
- **Admission cuts the queue**: with the third device `ρ` stays below ~0.9,
  because the dispatcher refuses the frames that would not finish in time.
  The real queue is shorter than the unconstrained one the formula describes:
  the likely main cause of the factor 2. A queue model with limited capacity
  is the next step only if the replay shows the overestimate leads to wrong
  decisions (verification 2, phase 3.8).
- With one device the queue never forms (regular arrivals, window of 3
  frames): verifying the model needs several devices.

**Limits**: one split point (6) and one worker size (2 × 2); clients and
worker on the same machine, so CPU contention adds noise to `S` (`cs`) and
to the device's times.
