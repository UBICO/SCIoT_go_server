import { MantineProvider } from '@mantine/core'
import { Notifications } from '@mantine/notifications'
import { render } from '@testing-library/react'
import type { ReactNode } from 'react'
import { createMemoryRouter, type RouteObject } from 'react-router'
import { RouterProvider } from 'react-router/dom'
import type { AdminDecision, AdminDevice, ServerStatus } from '../api/types'
import { type Connection, LiveContext } from '../live/context'
import type { EventStream } from '../live/LiveProvider'
import { type LiveState, liveReducer, initialLiveState } from '../live/state'

// Dati e helper condivisi dai test. Le forme seguono gli esempi di
// docs/ADMIN_API.md.

export function status(over: Partial<ServerStatus> = {}): ServerStatus {
  return {
    load: { cpu_percent: 12.5, goroutines: 20 },
    devices: 1,
    models: 1,
    started_at: '2026-11-19T10:00:00Z',
    uptime_seconds: 3725,
    ...over,
  }
}

// device: un device con un heartbeat a lastSeen. Senza heartbeat:
// device({ applied_policy_version: 0, telemetry: null }).
export function device(over: Partial<AdminDevice> = {}): AdminDevice {
  return {
    device_id: 'pi_a',
    device_type: 'python',
    session_id: 'FK2FWZX4OHX2WI5CVT25S67B3F',
    model_key: 'yolo_pico_320',
    policy: { mode: 'SPLIT', split_point: 5, policy_version: 2, max_inflight: 2 },
    pinned: false,
    last_offloading_layer: 7,
    max_inflight_max: 3,
    policy_since: '2026-11-19T12:00:00Z',
    applied_policy_version: 2,
    registered_at: '2026-11-19T11:59:00Z',
    last_seen: '2026-11-19T12:00:10Z',
    telemetry: {
      inference_count: 30,
      inference_time_ms: 40.5,
      inference_time_max_ms: 61.2,
      cpu_load: 0.42,
      rtt_ms: 12.1,
      memory_available_bytes: 812345344,
      answer_ms: 9.3,
    },
    network: { bandwidth_bps: 11e6, latency_ms: 2.5, not_limiting: false, valid_samples: 40, discarded_samples: 1 },
    last_decision: null,
    ...over,
  }
}

export function decision(at: string, over: Partial<AdminDecision['decision']> = {}): AdminDecision {
  return {
    at,
    decision: {
      kind: 'decision',
      ts: '0001-01-01T00:00:00Z',
      t_ms: 0,
      device_id: 'pi_a',
      algorithm: 'costmodel',
      current: { mode: 'LOCAL', split_point: null, policy_version: 1 },
      mode: 'SPLIT',
      split_point: 5,
      max_inflight: 2,
      changed: true,
      reason: 'split 5 is faster',
      predictions: [
        { split_point: 0, local: true, total_ms: 80, device_ms: 80, upload_ms: 0, wait_ms: 0, service_ms: 0, suffix_ms: 0, server_ms: 0, local_ms: 80, frames_per_s: 12, server_frames_per_s: 0, server_share: 0, rho: null, saturated: false },
        { split_point: 5, window: 2, local: false, total_ms: 50, p95_ms: 70, device_ms: 20, upload_ms: 10, wait_ms: 5, service_ms: 15, suffix_ms: 15, server_ms: 20, local_ms: 80, frames_per_s: 20, server_frames_per_s: 20, server_share: 1, bound: 'device', rho: 0.4, saturated: false },
      ],
      ...over,
    },
  }
}

// liveState: lo stato che produrrebbe lo stream con questi eventi.
export function liveState(devices: AdminDevice[], st: ServerStatus | null = status()): LiveState {
  let s = initialLiveState
  if (st !== null) {
    s = liveReducer(s, { type: 'status', status: st })
  }
  for (const d of devices) {
    s = liveReducer(s, { type: 'device', device: d })
  }
  return s
}

// renderRoutes: monta delle route in un router in memoria, con Mantine.
export function renderRoutes(routes: RouteObject[], path: string) {
  const router = createMemoryRouter(routes, { initialEntries: [path] })
  const utils = render(
    <MantineProvider env="test">
      <Notifications />
      <RouterProvider router={router} />
    </MantineProvider>,
  )
  return { router, ...utils }
}

// renderLive: una pagina con uno stato live fisso (senza stream), alla
// route path con il pattern pattern.
export function renderLive(page: ReactNode, state: LiveState, { path = '/', pattern = '/', connection = 'open' as Connection } = {}) {
  return renderRoutes(
    [
      { path: pattern, element: <LiveContext value={{ state, connection }}>{page}</LiveContext> },
      { path: '*', element: <p>elsewhere</p> },
    ],
    path,
  )
}

// FakeStream: un EventSource finto. I test chiamano emit/fail come farebbe
// il server.
export class FakeStream implements EventStream {
  static last: FakeStream | null = null
  static opened = 0

  readyState = 0
  onopen: ((ev: Event) => void) | null = null
  onerror: ((ev: Event) => void) | null = null
  closed = false
  url: string
  private listeners = new Map<string, ((ev: MessageEvent<string>) => void)[]>()

  constructor(url: string) {
    this.url = url
    FakeStream.last = this
    FakeStream.opened++
  }

  addEventListener(type: string, listener: (ev: MessageEvent<string>) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener])
  }

  close() {
    this.closed = true
    this.readyState = 2
  }

  open() {
    this.readyState = 1
    this.onopen?.(new Event('open'))
  }

  emit(type: string, data: unknown) {
    const ev = new MessageEvent<string>(type, { data: JSON.stringify(data) })
    for (const l of this.listeners.get(type) ?? []) {
      l(ev)
    }
  }

  // fail: readyState 0 = il browser riprova; 2 = risposta di errore.
  fail(readyState: 0 | 2) {
    this.readyState = readyState
    this.onerror?.(new Event('error'))
  }

  static reset() {
    FakeStream.last = null
    FakeStream.opened = 0
  }
}

// jsonResponse: una risposta di fetch.
export function jsonResponse(status: number, body?: unknown): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
  })
}
