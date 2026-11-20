import { act, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { routes } from './router'
import { device, FakeStream, jsonResponse, renderRoutes, status } from './test/fixtures'

// Le route vere, con fetch ed EventSource finti: il percorso completo login
// → dashboard → logout.

beforeEach(() => {
  FakeStream.reset()
  vi.stubGlobal('EventSource', FakeStream)
})
afterEach(() => vi.unstubAllGlobals())

const unauthorized = () => jsonResponse(401, { error: { code: 'unauthorized', message: 'x' } })

describe('routes', () => {
  it('sends to the login without a session', async () => {
    vi.stubGlobal('fetch', vi.fn().mockImplementation(async () => unauthorized()))
    const { router } = renderRoutes(routes, '/devices/pi_a')
    expect(await screen.findByRole('button', { name: 'Log in' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/login')
  })

  it('opens / as the devices list, fed by the stream', async () => {
    vi.stubGlobal('fetch', vi.fn().mockImplementation(async () => jsonResponse(200, status())))
    const { router } = renderRoutes(routes, '/')
    await screen.findByText('connecting') // il layout c'è, lo stream è aperto
    expect(router.state.location.pathname).toBe('/devices')

    act(() => {
      FakeStream.last!.open()
      FakeStream.last!.emit('status', status())
      FakeStream.last!.emit('device', device())
    })
    expect(screen.getByRole('heading', { name: 'Devices' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'pi_a' })).toBeInTheDocument()
    expect(screen.getByText('live')).toBeInTheDocument()
  })

  it('shows a device page from its URL', async () => {
    vi.stubGlobal('fetch', vi.fn().mockImplementation(async () => jsonResponse(200, status())))
    renderRoutes(routes, '/devices/pi_a')
    await screen.findByText('connecting')
    act(() => {
      FakeStream.last!.emit('status', status())
      FakeStream.last!.emit('device', device())
    })
    expect(await screen.findByRole('heading', { name: 'pi_a' })).toBeInTheDocument()
  })

  it('logs out', async () => {
    const f = vi.fn().mockImplementation(async (url: string) => (url.endsWith('/logout') ? jsonResponse(204) : jsonResponse(200, status())))
    vi.stubGlobal('fetch', f)
    const { router } = renderRoutes(routes, '/devices')
    await userEvent.click(await screen.findByRole('button', { name: 'Log out' }))
    expect(await screen.findByRole('button', { name: 'Log in' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/login')
    expect(f).toHaveBeenCalledWith('/api/v1/admin/logout', { method: 'POST' })
  })

  it('opens a read-only page, and the login when its loader gets 401', async () => {
    let models = 200
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation(async (url: string) => {
        if (url === '/api/v1/admin/models') {
          return models === 200 ? jsonResponse(200, { models: [] }) : unauthorized()
        }
        return jsonResponse(200, status())
      }),
    )
    renderRoutes(routes, '/models')
    expect(await screen.findByText('No model in the registry.')).toBeInTheDocument()

    models = 401
    const { router } = renderRoutes(routes, '/models')
    expect(await screen.findByRole('button', { name: 'Log in' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/login')
  })

  it('explains when the server is down', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
    renderRoutes(routes, '/devices')
    expect(await screen.findByText(/does not answer/)).toBeInTheDocument()
  })
})
