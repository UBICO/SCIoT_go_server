import { act, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { device, FakeStream, jsonResponse, renderRoutes, status } from '../test/fixtures'
import { useLive } from './context'
import { LiveProvider, RETRY_MS } from './LiveProvider'

// Probe mostra quello che LiveProvider espone.
function Probe() {
  const { state, connection } = useLive()
  return (
    <p>
      {connection} · devices {Object.keys(state.devices).join(',') || 'none'} · status {state.status === null ? 'no' : state.status.devices}
    </p>
  )
}

const open = (url: string) => new FakeStream(url)

function mount() {
  return renderRoutes(
    [
      { path: '/', element: <LiveProvider open={open}><Probe /></LiveProvider> },
      { path: '/login', element: <p>login page</p> },
    ],
    '/',
  )
}

function stream(): FakeStream {
  const s = FakeStream.last
  if (s === null) {
    throw new Error('no stream opened')
  }
  return s
}

beforeEach(() => FakeStream.reset())
afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('LiveProvider', () => {
  it('opens the events stream and dispatches its events', async () => {
    mount()
    expect(stream().url).toBe('/api/v1/admin/events')
    expect(screen.getByText(/connecting · devices none · status no/)).toBeInTheDocument()

    act(() => {
      stream().open()
      stream().emit('status', status({ devices: 2 }))
      stream().emit('device', device())
      stream().emit('device', device({ device_id: 'pi_b' }))
    })
    expect(screen.getByText('open · devices pi_a,pi_b · status 2')).toBeInTheDocument()
  })

  it('shows connecting while the browser retries by itself', () => {
    mount()
    act(() => {
      stream().open()
      stream().fail(0)
    })
    expect(screen.getByText(/^connecting/)).toBeInTheDocument()
    expect(FakeStream.opened).toBe(1) // nessuna riapertura: la fa il browser
  })

  it('goes to the login when the session has expired', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(401, { error: { code: 'unauthorized', message: 'x' } })))
    mount()
    act(() => stream().fail(2))
    expect(await screen.findByText('login page')).toBeInTheDocument()
    expect(stream().closed).toBe(true)
  })

  it('reopens the stream after a server error', async () => {
    // Solo setTimeout finto: il resto (promesse, router) resta vero. Niente
    // waitFor di Testing Library con i timer finti di vitest: non li
    // riconosce e aspetterebbe per sempre.
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(200, status())))
    mount()
    const first = stream()
    await act(async () => first.fail(2)) // risolve anche GET /status
    expect(screen.getByText(/^closed/)).toBeInTheDocument()

    await act(async () => {
      await vi.advanceTimersByTimeAsync(RETRY_MS - 1)
    })
    expect(FakeStream.opened).toBe(1)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1)
    })
    expect(FakeStream.opened).toBe(2)
    expect(first.closed).toBe(true)
    expect(screen.getByText(/^connecting/)).toBeInTheDocument()
  })

  it('closes the stream on unmount', () => {
    const { unmount } = mount()
    const s = stream()
    unmount()
    expect(s.closed).toBe(true)
  })
})
