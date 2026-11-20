# Admin API

The API of the admin port: the contract between the server and the dashboard
(and any script that controls the server). It is **not** part of the device
protocol ([PROTOCOL.md](PROTOCOL.md)): devices never use it.

The admin port only reads what the server already computes and calls the
existing admin actions (pin, unpin), edits `config.json` and restarts the
server to apply it, builds model directories with `sciot-model build`, writing
who did them to a change log. Recordings, models and the change log are
read-only. It
adds no metric and changes neither the protocol nor the offloading algorithm. Without `-admin-addr` it does not
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
  The server removes it from its environment after reading it, so the
  worker and the model builds do not inherit it; a restart from the
  dashboard passes it to the new program.
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
| `invalid_config` | 400 | the `config.json` text would not start the server, or changes `worker.command` |
| `config_changed` | 409 | `config.json` changed since you read it (`base_hash` does not match) |
| `build_running` | 409 | a model build is already running (one at a time) |
| `model_exists` | 409 | the model folder `models_dir/<model_key>` already exists |
| `not_running` | 409 | cancelling a build that has ended |
| `too_large` | 413 | a model build upload larger than 1 GiB |

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
  "last_offloading_layer": 7, "max_inflight_max": 2,
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
| `last_offloading_layer` | the model's: a pin accepts `split_point` from `-1` to `last_offloading_layer - 1` |
| `max_inflight_max` | the largest `max_inflight` a pin accepts (the stream's maximum); `null` without data plane, where there is no window |
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
with `-admin` ([PROTOCOL.md](PROTOCOL.md#post-apiv1admindevicesdevice_idpolicy-opt-in)). Every
successful pin and unpin goes to the change log.

```bash
curl -X POST -H "Authorization: Bearer $SCIOT_ADMIN_TOKEN" \
  http://127.0.0.1:8081/api/v1/admin/devices/pi_a/policy -d '{"mode":"SPLIT","split_point":5}'
curl -X DELETE -H "Authorization: Bearer $SCIOT_ADMIN_TOKEN" \
  http://127.0.0.1:8081/api/v1/admin/devices/pi_a/policy
```

### `GET /api/v1/admin/config`

The `config.json` the server was started with (`-config`), as text, to edit
it from the dashboard. A symbolic link is followed: `path` is the real file.

```json
{"path": "/srv/sciot/config.json", "content": "{\n  \"server\": {},\n …}\n",
 "hash": "725d660a…", "modified_since_start": false}
```

| Field | Meaning |
|---|---|
| `hash` | sha256 of `content`: send it back as `base_hash` when saving |
| `modified_since_start` | the file is not the one the server started with (saved from the dashboard or edited by hand): the server runs with the old one until it restarts |

### `POST /api/v1/admin/config/validate`

```json
{"content": "{ … the whole config.json … }"}
```

Runs on the text the same checks as the server's start (decoding with unknown
keys rejected, models and their hashes, every section; `parseConfig` in
`cmd/server/config.go`), plus one rule: `worker.command` cannot differ from
the file in use. `200 {"valid": true}`, or `400 invalid_config` with the
same message the server would log at start:

```json
{"error": {"code": "invalid_config", "message": "stream config: stream.max_inflight must be >= 1, got 0"}}
```

`worker.command` is a command line the server runs: if the dashboard could
change it, the admin token would give a shell on the machine. Change it by
hand on the server.

Paths in the config (models, `record.dir`) are checked on the server's
machine, relative to the server's working directory, as at start.

### `PUT /api/v1/admin/config`

```json
{"content": "{ … }", "base_hash": "<hash from GET>"}
```

In order: `409 config_changed` if the file's hash is not `base_hash` (someone
changed it after your `GET`: reload and apply your edits again); the same
checks as `validate` (`400 invalid_config`); if the text is the same as the
file, nothing is written; otherwise a backup of the current file, then the new
file, then a line in the change log. `200`:

```json
{"changed": true, "hash": "bab942fa…", "backup": "config.json.20261119-150405.bak",
 "restart_required": true}
```

- The running server does not change: the new config applies at the next
  start (`restart_required`).
- The backup is `<file>.<YYYYMMDD-HHMMSS>.bak` (local time) next to the file,
  `0600`, never overwritten. Backups are not deleted.
- The file is replaced atomically (a temporary file in the same folder,
  synced, renamed over it): a reader sees the old file or the new one, never
  half of it. It keeps its permissions; its owner becomes the server's user.
- `500` if the backup or the write fails: the file is left as it was.

### `POST /api/v1/admin/restart`

Restarts the server to apply `config.json`, without a body. First the file is
checked with the same checks as the start: a file that would not start the
server (edited by hand) gets `400 invalid_config` and the server keeps
running, since it would otherwise stop and not come back. Then a `restart`
line goes to the change log, the answer is `202 {"restarting": true}`, and the
server stops as on `SIGTERM` (streams closed with 1001, both ports, decisions,
recording, worker) and starts again in the same process (`syscall.Exec`: same
PID, same flags and environment, so the same `SCIOT_ADMIN_TOKEN`). Linux and
macOS only.

What a restart means:

- devices fall back to `LOCAL` while the server is down, get
  `session_expired` and register again (the Python client does both by
  itself); pins are lost, since a new registration starts unpinned;
- a new recording file starts (`record.dir`);
- dashboard sessions are in memory: log in again.

`404` without config editing (no check possible); `500` if
`config.json` cannot be read (no restart).

### `GET /api/v1/admin/models`

The models of the registry, sorted by `model_key`: the same objects as the
device port's `GET /api/v1/models` ([PROTOCOL.md](PROTOCOL.md)), plus
`devices`, how many registered devices use the model.

```json
{"models": [{"model_hash": "…", "model_key": "yolo_pico_320", "num_layers": 8,
  "last_offloading_layer": 7, "input": {"shape": [1, 320, 320, 3], "dtype": "float32"},
  "postprocess": {"decoder": "yolo_pro", "labels": ["bicycle", "stop sign"], …},
  "devices": 2}]}
```

### `GET /api/v1/admin/recordings`

The recordings in `record.dir` ([RECORDING.md](RECORDING.md)), newest first.
Only files named `sciot-YYYYMMDD-HHMMSS.jsonl` are listed (not the change
log, not other files).

```json
{"recording": true, "dir": "/srv/sciot-records", "files": [
  {"name": "sciot-20261120-091811.jsonl", "size_bytes": 2500000,
   "modified": "2026-11-20T09:30:00+01:00", "current": true}]}
```

`current` is the file of this start, still being written. Without a `record`
section: `{"recording": false, "files": []}`. Nothing can be deleted from
here: recordings are experiment data, delete them by hand on the machine.

### `GET /api/v1/admin/recordings/{name}`

Downloads a recording (`application/x-ndjson`, as an attachment). `Range` is
supported, so an interrupted download of a large file can resume. `404
not_found` for a name that is not a recording name (no path outside
`record.dir` can be reached), a missing file, or without recording.

The file being written may end with half a line (the recorder writes through
a buffer): the download stops at the last complete line, so `sciot-replay`
can read it. A finished file is served whole.

### `GET /api/v1/admin/changelog`

The last lines of the change log (below), newest first, as written in the
file: `?limit=N`, 1 to 1000, default 200 (`400 invalid_request` otherwise).
Lines that are not valid JSON (half a line after a crash) are skipped.

```json
{"enabled": true, "entries": [{"ts": "…", "who": "francesco", "action": "pin", …}]}
```

Without a `record` section: `{"enabled": false, "entries": []}` (actions are
then only in the server's log).

### Model builds: `/api/v1/admin/models/builds`

Builds a model directory from the two Edge Impulse exports with
`sciot-model build` ([MODELS.md](MODELS.md)), on the server, into
`models_dir/<model_key>`. Enabled by `"models_dir"` in `config.json` (an
existing folder); without it `GET` answers `{"enabled": false}` and the other
routes `404`. The command is fixed in the server (`uv run --project worker
sciot-model build`, run from the server's working directory like the worker),
never read from the config: the only arguments are the uploaded files and
validated options. One build at a time, kept in memory (the last 20): a
restart forgets the jobs, not the built models. A running build is stopped
when the server stops.

`GET …/builds`: `{"enabled": true, "models_dir": "models", "builds": [ … ]}`,
newest first, without logs.

`POST …/builds`, `multipart/form-data`, up to 1 GiB (this route only; the
read deadline is 15 minutes):

| Field | |
|---|---|
| `model_key` | the key, also the folder name: `[a-z0-9][a-z0-9_-]{0,63}` |
| `savedmodel` | one file, the "TensorFlow SavedModel" export (`.zip`) |
| `cpp_export` | one file, the "C++ library" export (`.zip`) |
| `sample` | 0 to 10 images for the verification (strongly recommended) |
| `max_segments` | optional, 0 to 64 (`--max-segments`; 0 = all automatic cuts) |
| `cut` | repeatable, a tensor name for `--cut` |

Uploaded files are saved under fixed names (never the browser's) and deleted
when the job ends. `202` with the job; `400 invalid_request`, `409
build_running`, `409 model_exists` (no `--overwrite` from here: a model in
use would change hash under the devices), `413 too_large`, `500` if the
command cannot start.

`GET …/builds/{id}?since=N`: the job and its log from line `N` (`next` is the
`since` of the following request), so a page follows the log by polling:

```json
{"id": "yolo_small_320-1", "model_key": "yolo_small_320", "output": "models/yolo_small_320",
 "who": "francesco", "state": "succeeded", "started_at": "…", "finished_at": "…",
 "exit_code": 0, "model_hash": "35cc5d15…", "log_lines": 42, "log": ["…"], "next": 42}
```

`state`: `running`, `succeeded` (exit 0 and a `model_hash` line), `failed`,
`cancelled`. The log has stdout and stderr, up to 10000 lines.

`DELETE …/builds/{id}`: cancels a running build (SIGTERM to the process
group, SIGKILL after 5 s); `202`, or `409 not_running`. `sciot-model build`
writes the model folder only at its last step: a cancel during that step may
leave a partial folder, which then makes a new build with the same key fail
with `model_exists`; delete it by hand.

The build process gets the server's environment without
`SCIOT_ADMIN_TOKEN`, plus `PYTHONUNBUFFERED=1` (so the log arrives line by
line).

The built model is not loaded: add `{"model_key", "tflite_dir"}` to
`models` in `config.json` (`PUT /api/v1/admin/config`) and restart.

## Change log

Who changed what: every successful admin action (pin, unpin, config save,
restart, model build and its cancellation) is written to the server log with
who did it:

```text
admin pin: device=pi_a session=FK2F… policy=SPLIT v4 by="francesco"
admin config: path=/srv/sciot/config.json backup=config.json.20261119-150405.bak by="francesco"
admin restart: config=/srv/sciot/config.json hash=bab942fa… by="francesco"
```

With a `record` section in `config.json`, the same action is also a line of
`<record.dir>/sciot-audit.jsonl`: one JSON object per line, one file for
every start (appended), readable only by the server's user (`0600`).

```json
{"ts": "2026-11-19T14:04:05.123Z", "who": "francesco", "action": "pin",
 "device_id": "pi_a", "session_id": "FK2F…",
 "policy": {"mode": "SPLIT", "split_point": 3, "policy_version": 4}, "pinned": true}
{"ts": "2026-11-19T15:07:58.505Z", "who": "francesco", "action": "config",
 "config": {"path": "/srv/sciot/config.json", "backup": "config.json.20261119-160758.bak", "hash": "bab942fa…"}}
```

| Field | Meaning |
|---|---|
| `ts` | server time, UTC |
| `who` | the `name` of the login; `token` for a request with `Authorization: Bearer`; `anonymous` on an admin port without token and without login; `device port` for the unauthenticated `-admin` endpoints on the device port |
| `action` | `pin`, `unpin`, `config`, `restart`, `model_build`, `model_build_cancel` |
| `device_id`, `session_id`, `policy`, `pinned` | pin and unpin: the device, its policy and pin after the action (after an unpin the policy is unchanged) |
| `build` | model build: `id`, `model_key`, `output` (its result is in the server log and in the job) |
| `config` | config save: the file written (`path`), the backup of the previous content (`backup`, in the same folder), sha256 of the new content (`hash`); restart: the file the server starts again with and its sha256 (no `backup`) |

Refused actions (invalid policy, unknown device, invalid config, conflict, a
restart refused because of the config) and a config save that changes nothing
are not logged. The change
log is not part of the recording ([RECORDING.md](RECORDING.md)): it says who
acted, not what happened to the frames.

## Security notes

- The admin port speaks plain HTTP: on a network, the token and the cookie
  can be read by anyone on the path. To reach a remote server, keep
  `-admin-addr 127.0.0.1:8081` and use an SSH tunnel:
  `ssh -L 8081:127.0.0.1:8081 <server>`, then open `http://127.0.0.1:8081`.
- There is no limit on login attempts: use a random token
  (`openssl rand -base64 32`), not a word.
- Under systemd, put the token in an `EnvironmentFile` readable only by the
  service user (`chmod 600`), not in the unit file.
