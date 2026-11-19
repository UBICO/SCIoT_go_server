# Admin API

The API of the admin port: the contract between the server and the dashboard
(and any script that controls the server). It is **not** part of the device
protocol ([PROTOCOL.md](PROTOCOL.md)): devices never use it.

The admin port only reads what the server already computes and calls the
existing admin actions (pin, unpin). It adds no metric and changes neither the
protocol nor the offloading algorithm. Without `-admin-addr` it does not
exist and the server runs as before.

The source of truth for the JSON shapes is `internal/server/admin.go` and
`internal/server/admin_auth.go`; `internal/server/admin_test.go` pins them.

## Enabling it

```bash
export SCIOT_ADMIN_TOKEN="$(openssl rand -base64 32)"
go run ./cmd/server -config config.json -addr :8080 -admin-addr 127.0.0.1:8081
```

- `-admin-addr` (default empty = no admin port) is a second HTTP listener,
  separate from the device port (`-addr`). Admin routes are not served on the
  device port, and device routes are not served on the admin port.
- The admin port also serves the dashboard: every path outside `/api/` is a
  page of it ([DASHBOARD.md](DASHBOARD.md)). The pages need no credentials
  (the data is behind the API); a binary built without the dashboard serves a
  page that says how to build it.
- The token comes from the environment variable `SCIOT_ADMIN_TOKEN`, never
  from `config.json` (the file is copied around and may end up in git).
- At start-up the server refuses to run (`admin: …` in the log) when:
  - the token is shorter than 16 characters;
  - there is no token and `-admin-addr` is not a loopback address
    (`127.0.0.0/8`, `::1`, `localhost`). `:8081` and `0.0.0.0:8081` listen on
    every interface.
- With no token on loopback the admin port has **no authentication** (a
  warning in the log): anyone on the machine can use it.
- The old `-admin` flag (`server.admin`) is unchanged: it serves the policy
  endpoints on the device port without authentication. With both on, the log
  warns that the token does not protect the policy.

## Authentication

Every route except `POST /api/v1/admin/login` needs one of:

- `Authorization: Bearer <token>`, for curl and scripts;
- the session cookie `sciot_admin` from the login, for the browser
  (`EventSource` cannot send headers; a cookie is sent automatically).

When the request has an `Authorization` header, the server decides on that
header only: a wrong token is refused even with a valid cookie, so a broken
script does not pass on a stale browser session.

The cookie holds a random session id (32 bytes, base64url), not the token.
It is `HttpOnly` (page scripts cannot read it), `SameSite=Strict` (the browser
does not send it from other sites), `Path=/`, and `Secure` only over TLS.
Sessions live in memory and last **12 hours**: after that, or after a server
restart, log in again.

Requests that change state (`POST`, `PUT`, `DELETE`) sent by a browser from
another origin are refused with `403` (Go's `http.CrossOriginProtection`,
from the `Sec-Fetch-Site` and `Origin` headers). curl and scripts do not send
those headers and are not affected.

Missing or wrong credentials:

```json
{"error": {"code": "unauthorized", "message": "missing or invalid admin credentials"}}
```

Errors have the same shape and the same rules as in the device protocol
([PROTOCOL.md](PROTOCOL.md#conventions)): JSON bodies, unknown keys rejected,
bodies limited to 64 KiB, branch on `code`.

| `code` | Status | Meaning |
|---|---|---|
| `unauthorized` | 401 | no credentials, wrong token, expired or unknown session |
| `invalid_request` | 400 | malformed JSON, unknown key, missing field, invalid policy |
| `device_not_found` | 404 | the device is not registered |
| `not_found` | 404 | no such endpoint under `/api/` on the admin port (device routes included) |

A cross-origin browser request gets a plain-text `403`, not this JSON.

## Endpoints

All paths are on the admin port.

### `POST /api/v1/admin/login`

```json
{"token": "<SCIOT_ADMIN_TOKEN>", "name": "francesco"}
```

`name` (required, not empty) is who is logging in: with one shared token it is
the only way to tell who changed what. `200` with `Set-Cookie: sciot_admin=…`:

```json
{"name": "francesco", "expires_at": "2026-11-20T00:33:08.436273+01:00"}
```

Errors: `400 invalid_request` (malformed body, empty `name`), `401
unauthorized` (wrong token). Without a configured token any token is accepted.

### `POST /api/v1/admin/logout`

Ends the cookie's session and clears the cookie. `204`.

### `GET /api/v1/admin/status`

The same body as the device port's `GET /api/v1/server/status`
([PROTOCOL.md](PROTOCOL.md#get-apiv1serverstatus)).

### `GET /api/v1/admin/devices`

The registered devices, sorted by `device_id`; `{"devices": []}` when there
are none. A device just registered, before its first heartbeat:

```json
{"devices": [{
  "device_id": "pi_a", "device_type": "python",
  "session_id": "FK2FWZX4OHX2WI5CVT25S67B3F", "model_key": "yolo_pico_320",
  "policy": {"mode": "LOCAL", "split_point": null, "policy_version": 1},
  "pinned": false,
  "policy_since": "2026-11-19T12:33:08.420258+01:00",
  "applied_policy_version": 0,
  "registered_at": "2026-11-19T12:33:08.420258+01:00",
  "last_seen": "2026-11-19T12:33:08.420258+01:00",
  "telemetry": null, "network": null, "last_decision": null}]}
```

| Field | Meaning |
|---|---|
| `policy` | the server's current policy for the device |
| `pinned` | the policy was set by the admin: the algorithm does not change it ([OFFLOAD.md](OFFLOAD.md#admin)) |
| `policy_since` | when the current policy came into force |
| `applied_policy_version` | the version the device said it applies in its last heartbeat; `0` = no heartbeat yet |
| `last_seen` | the device's last contact (registration or heartbeat) |
| `telemetry` | the `telemetry` object of the last heartbeat, as the device sent it ([PROTOCOL.md](PROTOCOL.md#post-apiv1heartbeat)); `null` until `applied_policy_version` > 0 |
| `network` | the network measured by the server on the device's stream, the same object as `network` in the recording's heartbeat ([RECORDING.md](RECORDING.md#network)); `null` without data plane or before the device opens the stream |
| `last_decision` | the algorithm's last decision for the device: `at` (server time) and `decision`, the same object as a `decision` line of the recording ([RECORDING.md](RECORDING.md#decision--an-offloading-algorithms-choice-at-a-heartbeat)), with its `reason` and per-split `predictions`; `null` with the `static` algorithm, before the first decision, and after a new registration |

`telemetry` is aggregated over the device's last heartbeat window, so it
describes the second before `last_seen`. The measured end-to-end p95 is not
in it: the only p95 is the algorithm's prediction
(`last_decision.decision.predictions[].p95_ms`).

`decision.ts` and `decision.t_ms` are set by the recorder: without a `record`
section they are zero; use `at`.

### `GET /api/v1/admin/events`

Server-Sent Events (`Content-Type: text/event-stream`): the same objects as
`GET status` and `GET devices`, pushed as they change. The browser opens it
with `new EventSource("/api/v1/admin/events")` after the login (the cookie is
sent automatically); curl with `-N -H "Authorization: Bearer …"`.

```text
event: status
data: {"load": {...}, "devices": 1, "models": 1, ...}

event: device
data: {"device_id": "pi_a", "policy": {...}, "telemetry": {...}, ...}
```

| Event | `data` | When |
|---|---|---|
| `status` | the body of `GET /api/v1/admin/status` | on connect, then once per `heartbeat_interval_ms` |
| `device` | one element of `GET /api/v1/admin/devices` | on connect, one per device; then when the device has a new heartbeat (`last_seen`), a new policy or pin (`policy.policy_version`, `pinned`), a new session, or a new decision (`last_decision.at`) |

- On connect the stream starts with the full state: no `GET` is needed
  first, and nothing is lost between a `GET` and the subscription. After a
  reconnection the state is sent again in full; there are no event ids.
- Each connection reads the device store every `heartbeat_interval_ms / 4`
  and sends only the devices that changed. A heartbeat reaches the stream at
  most a quarter of the interval after it reached the server: with a 1 s
  interval, 30 heartbeats from a local fake device arrived 110–247 ms later
  (median 176 ms, 2026-11-19, server and client on one Mac). Two heartbeats
  of one device within the same quarter would show only the second.
- Devices are never removed from the store, so there is no "removed" event.
- The stream has no write timeout and ends when the client disconnects or
  the server stops.

### `POST /api/v1/admin/devices/{device_id}/policy`, `DELETE …`

Pin and unpin, with the same bodies, answers and errors as on the device port
with `-admin` ([PROTOCOL.md](PROTOCOL.md#post-apiv1admindevicesdevice_idpolicy-opt-in)).

```bash
curl -X POST -H "Authorization: Bearer $SCIOT_ADMIN_TOKEN" \
  http://127.0.0.1:8081/api/v1/admin/devices/pi_a/policy -d '{"mode":"SPLIT","split_point":5}'
curl -X DELETE -H "Authorization: Bearer $SCIOT_ADMIN_TOKEN" \
  http://127.0.0.1:8081/api/v1/admin/devices/pi_a/policy
```

## Security notes

- The admin port speaks plain HTTP: on a network, the token and the cookie
  can be read by anyone on the path. To reach a remote server, keep
  `-admin-addr 127.0.0.1:8081` and use an SSH tunnel:
  `ssh -L 8081:127.0.0.1:8081 <server>`, then open `http://127.0.0.1:8081`.
- There is no limit on login attempts: use a random token
  (`openssl rand -base64 32`), not a word.
- Under systemd, put the token in an `EnvironmentFile` readable only by the
  service user (`chmod 600`), not in the unit file.
