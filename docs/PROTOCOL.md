# SCIoT control-plane protocol — v1

This document is the contract between the SCIoT Go server and its clients
(the Python reference client today, the ESP32-S3-EYE later). It covers the
**control plane** only: pairing, model information, telemetry and the
execution policy (`LOCAL` / `SPLIT`). The data plane (activation tensors and
results) is not part of v1.

The source of truth for field names is `internal/api`; the tests in
`internal/api/types_test.go` pin the exact JSON.

## Conventions

- Transport: HTTP/1.1 with keep-alive. The server keeps idle connections open
  for 120 s, so a client sending a heartbeat every second reuses one TCP
  connection.
- Bodies are JSON (`Content-Type: application/json`). Unknown keys in a
  request are **rejected** (`400 invalid_request`): a typo never goes unnoticed.
- Request bodies are limited to 64 KiB.
- Every error has the same shape:

  ```json
  {"error": {"code": "model_not_found", "message": "unknown model_hash \"...\""}}
  ```

  Clients must branch on `code`; `message` is for humans and may change.

| `code` | Status | Meaning |
|---|---|---|
| `invalid_request` | 400 | malformed JSON, unknown key, body too large, missing or invalid field |
| `unsupported_protocol` | 400 | `protocol_version` is not 1 |
| `model_not_found` | 404 | the server does not serve this `model_hash` |
| `session_expired` | 409 | the device or session is not (or no longer) known |
| `device_not_found` | 404 | admin endpoint: the device is not registered |
| `internal_error` | 500 | server bug; details are in the server log only |

## Model identity

A model is identified by `model_hash`: the **MD5 of the concatenated contents
of all `*.tflite` segment files, ordered by file name** (lexicographic, so
`submodel_10` sorts before `submodel_2`; what matters is that client and
server use the same order). The client computes it on its local segments;
the server computes it at startup for every model in `config.json`. Matching
hashes guarantee that both sides split the same network at the same layers.

## Execution policy

```json
{"mode": "LOCAL", "split_point": null, "policy_version": 1}
{"mode": "SPLIT", "split_point": 5,    "policy_version": 2}
```

- `mode`: `LOCAL` (whole model on the device) or `SPLIT` (layers
  `0..split_point` on the device, the rest on the server).
- `split_point`: always present; `null` in `LOCAL`, an integer in
  `0..last_offloading_layer` in `SPLIT`.
- `policy_version`: increases every time the server changes the policy.
  It is **per session**: a new registration starts again from `LOCAL` v1.

Client rules:

1. Apply a received policy only if its `policy_version` is **greater** than
   the one already accepted. Equal or older versions are ignored (late or
   duplicated answers).
2. A policy takes effect from the **next inference**: an inference already
   started finishes with the policy it started with.
3. Keep "accepted policy" and "executed policy" separate. When the server is
   unreachable, or the client cannot execute `SPLIT` (no data plane yet), it
   executes `LOCAL` but keeps the accepted version. Overwriting the accepted
   policy with `LOCAL` would make the client ignore the same version forever
   once the server is back.

## Endpoints

### `POST /api/v1/register`

Pairs a device. A registration of an already known `device_id` **replaces**
its whole state (new session, policy back to `LOCAL` v1).

Request:

```json
{
  "protocol_version": 1,
  "device_id": "web_96",
  "device_type": "python",
  "model_hash": "2b319af7002697e06bf876905d72d6a5",
  "capabilities": {"raw_input_split": true}
}
```

`200`:

```json
{
  "session_id": "YVOFM5HEJSRDMW2JBQU4ADGDD6",
  "model": {"model_key": "fomo_96x96", "num_layers": 59,
            "last_offloading_layer": 58, "input_size_bytes": 18432},
  "policy": {"mode": "LOCAL", "split_point": null, "policy_version": 1},
  "heartbeat_interval_ms": 1000
}
```

Errors: `400 invalid_request`, `400 unsupported_protocol`, `404 model_not_found`.
On `model_not_found` the client should run LOCAL-only and not retry.

### `POST /api/v1/heartbeat`

Sent every `heartbeat_interval_ms` (from the registration answer). Carries the
telemetry of the **window** since the previous heartbeat and the policy the
client is executing (acknowledgement). The answer always contains the current
server policy.

Request:

```json
{
  "device_id": "web_96",
  "session_id": "YVOFM5HEJSRDMW2JBQU4ADGDD6",
  "policy_version": 1,
  "mode": "LOCAL",
  "telemetry": {
    "inference_count": 47,
    "inference_time_ms": 21.3,
    "inference_time_max_ms": 38.9,
    "cpu_load": 0.42,
    "rtt_ms": 3.1,
    "memory_available_bytes": 812345344
  }
}
```

- `policy_version` / `mode`: the policy the client is **executing**, not the
  last one received (they differ during a fallback or when `SPLIT` is
  downgraded).
- Telemetry:

  | Field | Required | Rule |
  |---|---|---|
  | `inference_count` | yes | inferences completed in the window, `>= 0`; `0` means the inference loop is stalled |
  | `inference_time_ms` | only if count > 0 | mean compute time of the window; absent if count is 0 |
  | `inference_time_max_ms` | only if count > 0 | `>= inference_time_ms` |
  | `cpu_load` | no | `0..1`, average over the window |
  | `rtt_ms` | no | duration of the **previous** heartbeat; absent after a failed one |
  | `memory_available_bytes` | no | `>= 0` |

  Optional fields are omitted when unknown: `0` is a value, not "absent".
  `NaN` and `Infinity` are not valid JSON and must never be sent.

`200`:

```json
{"policy": {"mode": "LOCAL", "split_point": null, "policy_version": 1}}
```

Errors: `400 invalid_request`, `409 session_expired`. On `session_expired`
(server restarted, or a newer registration of the same device) the client
falls back to `LOCAL` and registers again.

### `GET /api/v1/models`, `GET /api/v1/models/{model_hash}`

Models served by this server, sorted by `model_key`:

```json
{"models": [{"model_hash": "2b31...", "model_key": "fomo_96x96",
             "num_layers": 59, "last_offloading_layer": 58,
             "input_size_bytes": 18432}]}
```

The single-model form returns one entry, or `404 model_not_found`.

### `POST /api/v1/admin/devices/{device_id}/policy` (opt-in)

Forces the policy of a registered device; it is delivered with the next
heartbeat answer. Enabled only with `-admin` or `"admin": true` in
`config.json`, and **unauthenticated**: bind the server to localhost when
using it.

```json
{"mode": "SPLIT", "split_point": 5}
```

The server assigns the version (current + 1) and validates `split_point`
against the device's model. `200`:

```json
{"device_id": "web_96", "session_id": "YVOF...",
 "policy": {"mode": "SPLIT", "split_point": 5, "policy_version": 2}}
```

Errors: `400 invalid_request`, `404 device_not_found`.

### `GET /healthz`

`200 {"status":"ok"}`. Not versioned: it is for process monitoring, not for
clients.

## Typical session

```text
client                                   server
  │ POST /register (model_hash)            │
  │──────────────────────────────────────▶│ session S, LOCAL v1
  │◀──────────────────────────────────────│
  │                                        │
  │ every interval: POST /heartbeat        │
  │   (S, applied LOCAL v1, window stats)  │
  │──────────────────────────────────────▶│
  │◀──────────── policy (LOCAL v1) ────────│
  │              ...                       │
  │                                        │ policy changes → SPLIT@5 v2
  │ POST /heartbeat (applied LOCAL v1)     │
  │──────────────────────────────────────▶│
  │◀──────────── policy (SPLIT@5 v2) ──────│
  │ next inference uses SPLIT@5            │
  │              ...                       │
  │                                        │ server restarts
  │ POST /heartbeat (S)                    │
  │──────────────────────────────────────▶│
  │◀──────────── 409 session_expired ──────│
  │ fall back to LOCAL, register again     │
```
