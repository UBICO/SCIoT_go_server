# Dashboard

A web page to watch and configure the server: devices, their policy and
telemetry live, the algorithm's decisions. It is served by the server itself
on the admin port (`-admin-addr`), from files built into the binary: the
server machine needs neither Node nor extra files.

The dashboard is a client of the admin API ([ADMIN_API.md](ADMIN_API.md)) and
nothing else: it shows what the server already computes and calls the
existing admin actions. It adds no metric and changes neither the protocol
nor the offloading algorithm. Without `-admin-addr` there is no dashboard and
the server runs as before.

Status: devices list, device page with live charts, pin and unpin,
models and model builds, recordings, change log, `config.json` editing and
restart.

## Quick start

From a clone, with Go 1.27, Node 26 and [uv](https://docs.astral.sh/uv/)
(for the worker):

```bash
cd dashboard && npm ci && npm run build && cd ..
go build -o sciot-server ./cmd/server
cp config.example.json config.json
export SCIOT_ADMIN_TOKEN="$(openssl rand -base64 32)" && echo "$SCIOT_ADMIN_TOKEN"
./sciot-server -config config.json -addr :8080 -admin-addr 127.0.0.1:8081
```

Open `http://127.0.0.1:8081`, log in with your name and the token printed
above. `config.example.json` loads the two models in `models/` and starts the
worker. Some pages need a section in `config.json`:

| Page or feature | Needs |
|---|---|
| Recordings, Change log | `"record": {"dir": "<folder>"}` ([RECORDING.md](RECORDING.md)) |
| Build a model | `"models_dir": "<existing folder>"`, better outside the repository |
| Bandwidth chart, window in Pin | the `worker` section (data plane) |
| Decisions on the device page | `"offload": {"algorithm": "latency"}` ([OFFLOAD.md](OFFLOAD.md)) |

You can add them from the Config page itself, then restart from there.

## Using it

```bash
export SCIOT_ADMIN_TOKEN="$(openssl rand -base64 32)"
./sciot-server -config config.json -addr :8080 -admin-addr 127.0.0.1:8081
```

Open `http://127.0.0.1:8081`, enter a name (who you are: with one shared
token it is the only way to tell who changed what) and the token. The session
lasts 12 hours, or until the server restarts; then the page goes back to the
login by itself.

On a remote server keep `-admin-addr 127.0.0.1:8081` and open an SSH
tunnel: `ssh -L 8081:127.0.0.1:8081 <server>`, then `http://127.0.0.1:8081`
([ADMIN_API.md](ADMIN_API.md#security-notes)).

### As a service (systemd)

An example for a Linux server; adapt user, paths and `PATH` (uv is often in
`~/.local/bin`, which systemd does not see). The token lives in a file only
root can read, never in the unit:

```bash
sudo mkdir -p /etc/sciot
printf 'SCIOT_ADMIN_TOKEN=%s\n' "$(openssl rand -base64 32)" | sudo tee /etc/sciot/admin.env >/dev/null
sudo chmod 600 /etc/sciot/admin.env
sudo cat /etc/sciot/admin.env   # the token for the login
```

```ini
# /etc/systemd/system/sciot-server.service
[Unit]
Description=SCIoT server
After=network-online.target
Wants=network-online.target

[Service]
User=sciot
# The worker command and the relative paths of config.json start here.
WorkingDirectory=/opt/sciot-go-server
EnvironmentFile=/etc/sciot/admin.env
Environment=PATH=/home/sciot/.local/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=/opt/sciot-go-server/sciot-server -config config.json -addr :8080 -admin-addr 127.0.0.1:8081
Restart=on-failure
# Shutdown closes the streams, both ports and the worker: give it time.
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now sciot-server
journalctl -u sciot-server -f   # the server's log, change log lines included
```

A restart from the dashboard keeps the same process (same PID), so systemd
sees no restart. For the Config page to save, `User` must be able to write
`config.json` and its folder (the backup and the atomic write create files
next to it).

### Pages

- **Devices**: every registered device, sorted by id: type, model, the
  server's policy (`pinned` when set by the admin, `pending` when the device
  still applies an older version), mean inference time, last heartbeat, the
  reason of the last decision.
- **Device** (`/devices/<id>`):
  - the last heartbeat (telemetry as the device sent it) and the bandwidth
    measured by the server;
  - **Policy**: pin and unpin (below);
  - four charts, one point per heartbeat: latency (inference mean and max,
    answer, RTT, network latency), the server's policy (`LOCAL`, split
    points), device CPU, bandwidth;
  - the last decision: policy before → after, reason, and the cost model's
    prediction for every split (`p95` is the predicted one; the measured p95
    is not in the telemetry); the chosen row is bold;
  - the policy changes seen since the dashboard was opened (decisions that keep
    the policy are not listed: `latency` decides at every heartbeat).

- **Models**: the registry's models (key, hash, segments and valid splits,
  input, decoder, labels) and how many devices use each. **Build a model**
  (with `models_dir` in `config.json`): below.
- **Recordings**: the files of `record.dir`, newest first, with size and last
  write; a click downloads one. The file of this start is marked `recording`:
  its download stops at the last complete line, so `sciot-replay` reads it.
  Nothing is deleted from here.
- **Change log**: the last 200 admin actions (pin, unpin, config, restart)
  with time and who; needs a `record` section in `config.json`.
- **Config**: below.

### Build a model

With `"models_dir": "models"` in `config.json` (an existing folder), Models →
**Build a model** runs `sciot-model build` on the server ([MODELS.md](MODELS.md)):

1. the model key (also the folder name), the two Edge Impulse exports
   (SavedModel `.zip`, C++ library `.zip`, same model version), real images
   for the verification, and optionally `max segments` and `cuts`;
2. **Upload and build**: the page follows the log while the build runs, and
   can cancel it;
3. when it succeeds (`model_hash` in the log), **Add to config.json** opens
   the Config page with the model added to `models` (the file is reformatted
   with 2-space indentation: the diff shows everything that changes); Save,
   then restart the server.

One build at a time; the build loads the CPU for minutes, so do not build
during a measurement run. An existing model folder is refused (no overwrite
from the dashboard); a build cancelled while writing its folder may leave it
half written: delete it by hand before building the same key again. The list
of builds lives in the server's memory.

### Pin and unpin

A **pin** forces a policy on the device: `LOCAL`, or `SPLIT` at a split point
with a window. The server validates it, gives it a new version and sends it
with the next heartbeat; the offloading algorithm then leaves the device
alone. **Unpin** gives the device back to the algorithm: the policy stays until
the algorithm decides otherwise (with the `static` algorithm, it stays).
A new registration of the device starts unpinned. Details:
[OFFLOAD.md](OFFLOAD.md#admin).

The panel offers only valid values: split points from `-1` (the raw input,
the whole model on the server) to the model's `last_offloading_layer - 1`,
and windows up to the stream's maximum (`max` = no `max_inflight`, the
maximum). Without data plane there is no window. Both actions ask for a
confirmation, show the result as a notification, and go to the change log
with your login name ([ADMIN_API.md](ADMIN_API.md#change-log)). The page
shows the new policy when the stream sends the device again.

The form starts from the current policy and does not follow it afterwards: if
the algorithm changes the policy while you choose, your choice stays.

### Config

The **Config** page edits the server's `config.json` (the file given with
`-config`) for the next start; the running server does not change.

- **Validate** asks the server whether it would start with the text: the same
  checks as the start, with the same message it would log
  (`stream config: stream.max_inflight must be >= 1, got 0`). The editor also
  marks text that is not JSON when it loses focus.
- **Changes** shows the diff between the file and your text.
- **Save** shows the diff again and asks for a confirmation. The server
  checks the text again, keeps a backup of the current file next to it
  (`config.json.<YYYYMMDD-HHMMSS>.bak`), writes the new one atomically and
  adds a line to the change log with your name. Then restart the server to
  apply it (below).
- `worker.command` cannot be changed from here: it is a command the server
  runs. Edit it by hand on the server.
- If the file changed after you opened the page (edited by hand, or saved
  from another tab), saving stops with "The file changed on the server": your
  text stays in the editor, copy your edits, reload, apply them again.
- A yellow banner says when the file differs from the configuration the
  server started with.

**Restart server** (in the "Saved" notice and in the yellow banner) applies
the file. After a confirmation that lists what a restart means (devices in
`LOCAL` for a few seconds, then registered again; pins lost; a new recording
file; a new login), the server checks the file again and refuses to restart
if it would not start. Otherwise it stops in order and starts again in the
same process; the page waits for the new server (a different start time, or
the 401 of the lost session) and opens the login. After 60 s without an
answer it says so: check the server's log on the machine.

The header shows whether the live stream is connected (`live`,
`connecting`, `disconnected`) and the server's state (devices, CPU, uptime).

### What the charts show

The server sends only the current state of each device, never its history
(it keeps no extra state for the dashboard). The history is collected by the
browser tab: the charts start empty when the dashboard is opened or
reloaded, and keep the last 10 minutes. Two tabs have two histories. For the
history of a whole run use the recording ([RECORDING.md](RECORDING.md)) and
`sciot-replay`.

Ages ("3 s ago") compare the browser's clock with the server's `last_seen`:
with a remote server whose clock is off, they are off by the same amount.

## Common tasks

**Change a setting and apply it** (a threshold, the heartbeat interval, the
algorithm):

1. Config: edit the text; the diff below shows the change.
2. Validate: the server answers whether it would start with it, with the
   message it would log.
3. Save (a backup of the old file stays next to it).
4. Restart server, in the green notice; log in again.

**Force a split for an experiment and keep the data** (needs `record`):

1. Devices → the device → Policy: `SPLIT`, the split point, the window →
   Pin. The device applies it at its next heartbeat; the badge shows `pinned`.
2. Run the experiment. The algorithm leaves the device alone.
3. Unpin when done (the policy stays until the algorithm changes it).
4. Recordings → download the file of this start (marked `recording`), then
   analyse it offline: `go run ./cmd/sciot-replay algo <file>`
   ([COSTMODEL.md](COSTMODEL.md)).

A pin is lost when the device registers again or the server restarts.

**Add a new model** (needs `models_dir`):

1. Models → Build a model: key, the two Edge Impulse exports of the same
   model version, a few real images ([MODELS.md](MODELS.md)).
2. Follow the log until `succeeded`; note the `model_hash`.
3. Add to config.json → review the diff → Save → Restart server.
4. Copy the model folder (`models_dir/<key>`) to every device and point the
   client at it (`--model-dir`, [CLIENT.md](CLIENT.md)): a device registers
   with the `model_hash` of its own files, which must match the server's.

**See who changed what**: Change log (needs `record`), or the server's log
(`admin pin: … by="…"`).

## Building

The dashboard is in `dashboard/` (Vite, React, TypeScript). The build writes
into `internal/dashboard/dist/`, which `internal/dashboard/embed.go` includes
in the binary with `go:embed`:

```bash
cd dashboard && npm ci && npm run build && cd ..
go build -o sciot-server ./cmd/server
```

Cross-compiling for a Raspberry Pi or a Linux server works the same way: the
dashboard is plain files, so one build serves every platform.

```bash
cd dashboard && npm ci && npm run build && cd ..
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o sciot-server-arm64 ./cmd/server
```

Without the build (a fresh clone, the Go CI job, a machine without Node) the
server compiles and runs anyway: the admin port serves a page that says how
to build the dashboard, and the admin API works.

How that works: `go:embed` refuses to compile when the folder has no file,
and `dist/` is build output, not in git. So git keeps one file there,
`internal/dashboard/dist/embed-placeholder.txt` (`.gitignore` ignores
everything else in `dist/`). Vite empties `dist/` before every build; the same
file is in `dashboard/public/`, which Vite copies into `dist/`, so the build
writes it back unchanged and git stays clean. The server looks for
`index.html`: without it, it serves the "not built" page.

There is no `go:generate` step for the build: it would make `go generate
./...` need Node and the network.

## Development

### Setup

- Node 26 (LTS, `dashboard/.nvmrc`); npm comes with it.
- `cd dashboard && npm ci`

### Running

Two processes: the Go server with an admin port, and Vite's dev server,
which reloads the page on every save and forwards `/api` to the Go server
(`server.proxy` in `vite.config.ts`).

```bash
SCIOT_ADMIN_TOKEN=dev-token-0123456789 go run ./cmd/server -config config.json -addr 127.0.0.1:8080 -admin-addr 127.0.0.1:8081
```

```bash
cd dashboard && npm run dev
```

Open `http://localhost:5173`. To use a server on another address (for
example when 8081 is taken), start Vite with
`SCIOT_ADMIN_URL=http://127.0.0.1:18081 npm run dev`. For the browser the page
and the API are on one origin, so the session cookie and the cross-origin protection behave as
in production. Without devices the pages are empty: run a client
([CLIENT.md](CLIENT.md)), or any script that registers and sends heartbeats
([PROTOCOL.md](PROTOCOL.md)).

### Checks (same as CI, job `dashboard`)

```bash
cd dashboard
npm run lint      # eslint
npm test          # vitest, once; npm run test:watch to rerun on save
npm run build     # tsc -b (types, tests included), then vite build
```

### Layout

```text
dashboard/
  index.html            the page; Vite injects the bundle
  vite.config.ts        build into ../internal/dashboard/dist, dev proxy, vitest
  eslint.config.js      typescript-eslint, React hooks, React refresh
  public/               copied as is into the build (the go:embed placeholder)
  src/
    main.tsx            entry: Mantine provider, router
    router.tsx          routes, login check (loader)
    api/types.ts        JSON shapes of the admin API (mirror of the Go structs)
    api/client.ts       fetch wrappers, ApiError
    live/               the events stream: LiveProvider, reducer, history
    components/         TimeChart (uPlot), PolicyBadges, PolicyControl (pin), ConfigDiff,
                        RestartButton, BuildJob, chart data
    pages/              Login, Layout (header, menu), device list, device page, models,
                        build, recordings, change log, config
    configEdit.ts       edits to config.json made for the user (add a built model)
    format.ts, hooks.ts number/time formatting, useNow
    test/               vitest setup, fixtures, fake EventSource
internal/dashboard/
  embed.go              go:embed of dist/, the HTTP handler
  dist/                 build output (only the placeholder is in git)
```

Tests sit next to the code (`*.test.ts(x)`).

### How it is built

- **Libraries.** React 19, Mantine 9 (components and light/dark theme,
  following the system; `@mantine/notifications` for the results of actions;
  `JsonInput` as the config editor), jsdiff (`diff`, the config diff),
  react-router 8 ("data mode": routes as objects, loaders), uPlot (canvas
  charts, ~50 KB, light enough for 600 points per series updated every
  second). TypeScript 6: typescript-eslint does not
  support TypeScript 7 yet.
- **Routes and login.** `router.tsx` declares the routes. The protected area
  (`/` and its children) has a loader, `requireLogin`, that calls `GET
  /status` before any page: a 401 redirects to `/login`. URLs are real
  (`/devices/pi_a`): on a reload the browser asks the server for that path,
  and the Go handler answers with `index.html` for every path that is not a
  file, a device id with a dot included (`internal/dashboard/embed.go`); then
  the router picks the page. A missing file under `/assets/` (where Vite puts
  the hashed bundle) is a 404 instead, so a stale page never gets HTML for a
  script.
- **Live data.** `LiveProvider` (in the protected layout, so one connection
  per tab whatever the page) opens `EventSource("/api/v1/admin/events")`; the
  session cookie goes with it. Each `status` and `device` event goes through
  `liveReducer` (`live/state.ts`), which keeps the last state of each device
  and appends a point to its history only when `last_seen` changes (the
  stream also resends a device for a pin, a decision, or after a
  reconnection). On a dropped connection the browser reconnects by itself;
  on an error answer `EventSource` stops, so `LiveProvider` asks `GET /status`:
  401 → login, anything else → reopen after 3 s.
- **Charts.** `TimeChart` wraps uPlot, which is not a React component: it is
  created on a `<div>` once the width is known, then updated with `setData`
  and `setSize`, and created again only when its options change (theme,
  series). The series lists are module constants: a new list at every render
  would rebuild the chart every second.
- **Absent is not zero.** A value the device did not send is `null` in the
  history (a gap in the line) and `–` on screen, never 0, as in the rest of
  the project.
- **Comments** in TypeScript are in Italian, as in Go.

### Adding a page

1. A component in `src/pages/` (one component per file: React refresh).
2. A route in `routes` (`router.tsx`), under the protected `/` to require the
   login.
3. A menu entry in `nav` (`pages/Layout.tsx`).
4. A test next to it: `renderLive` (fixed live state) or `renderRoutes`
   (real routes, fake `fetch`) from `src/test/fixtures.tsx`.

### Changing the admin API

The Go structs are the source of truth (`internal/server/admin.go` and the
types it embeds). A field added, renamed or removed there changes, in the
same PR: `dashboard/src/api/types.ts`, the fixtures in
`dashboard/src/test/fixtures.tsx`, and [ADMIN_API.md](ADMIN_API.md).

### Tests

- vitest with jsdom; Testing Library for components
  (`@testing-library/react`, `user-event`, `jest-dom` matchers).
- `src/test/setup.ts` adds what jsdom lacks and Mantine needs (`matchMedia`,
  `ResizeObserver`, `document.fonts`), and cleans the DOM after each test.
- Text with braces goes into an input with `fireEvent.change`:
  `userEvent.type` reads `{` as a special key (see `ConfigPage.test.tsx`).
- `fetch` is replaced per test with `vi.stubGlobal`; `EventSource` with
  `FakeStream` (`open`, `emit`, `fail` drive it like the server would).
- Charts are not drawn in jsdom (no canvas, width 0): their data is tested
  through `toColumns`; the page tests check titles and messages.
- With fake timers (`vi.useFakeTimers`) do not use Testing Library's
  `waitFor`/`findBy`: they only recognise Jest's fake timers and wait
  forever. Advance the timers with `vi.advanceTimersByTimeAsync` inside
  `act`, then assert directly (see `LiveProvider.test.tsx`).

### npm cache

If `npm` fails with `EACCES` on `~/.npm` (files owned by root, from an old
npm), fix the owner once: `sudo chown -R $(id -u):$(id -g) ~/.npm`.
