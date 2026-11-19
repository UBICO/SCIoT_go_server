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

Status: devices list and device page with live charts. Next, in this order:
pin/unpin from the page, `config.json` editing with validation and restart,
recordings and models, model build jobs.

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

### Pages

- **Devices**: every registered device, sorted by id: type, model, the
  server's policy (`pinned` when set by the admin, `pending` when the device
  still applies an older version), mean inference time, last heartbeat, the
  reason of the last decision.
- **Device** (`/devices/<id>`):
  - the last heartbeat (telemetry as the device sent it) and the bandwidth
    measured by the server;
  - four charts, one point per heartbeat: latency (inference mean and max,
    answer, RTT, network latency), the server's policy (`LOCAL`, split
    points), device CPU, bandwidth;
  - the last decision: policy before → after, reason, and the cost model's
    prediction for every split (`p95` is the predicted one; the measured p95
    is not in the telemetry); the chosen row is bold;
  - the policy changes seen since the dashboard was opened (decisions that keep
    the policy are not listed: `latency` decides at every heartbeat).

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

Open `http://localhost:5173`. For the browser the page and the API are on
one origin, so the session cookie and the cross-origin protection behave as
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
    components/         TimeChart (uPlot), PolicyBadges, chart data
    pages/              Login, Layout (header, menu), device list, device page
    format.ts, hooks.ts number/time formatting, useNow
    test/               vitest setup, fixtures, fake EventSource
internal/dashboard/
  embed.go              go:embed of dist/, the HTTP handler
  dist/                 build output (only the placeholder is in git)
```

Tests sit next to the code (`*.test.ts(x)`).

### How it is built

- **Libraries.** React 19, Mantine 9 (components and light/dark theme,
  following the system), react-router 8 ("data mode": routes as objects,
  loaders), uPlot (canvas charts, ~50 KB, light enough for 600 points per
  series updated every second). TypeScript 6: typescript-eslint does not
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
  `ResizeObserver`), and cleans the DOM after each test.
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
