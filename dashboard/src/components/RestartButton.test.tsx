import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { jsonResponse, renderRoutes, status } from '../test/fixtures'
import { RestartButton } from './RestartButton'

afterEach(() => vi.unstubAllGlobals())

const OLD = '2026-11-20T09:00:00Z'
const NEW = '2026-11-20T09:05:00Z'

// server: un fetch finto. statusAnswers sono le risposte di GET /status in
// ordine (l'ultima si ripete); restart è la risposta di POST /restart.
function server(statusAnswers: (() => Response)[], restart: () => Response) {
  let i = 0
  const f = vi.fn<typeof fetch>().mockImplementation(async (input) => {
    if (String(input) === '/api/v1/admin/restart') {
      return restart()
    }
    const r = statusAnswers[Math.min(i, statusAnswers.length - 1)]!
    i++
    return r()
  })
  vi.stubGlobal('fetch', f)
  return f
}

const up = (startedAt: string) => () => jsonResponse(200, status({ started_at: startedAt }))
const down = () => {
  throw new TypeError('Failed to fetch')
}
const unauthorized = () => jsonResponse(401, { error: { code: 'unauthorized', message: 'x' } })
const accepted = () => jsonResponse(202, { restarting: true })

// Tempi brevi e timer veri: i timer finti di vitest bloccano userEvent e
// Testing Library.
function mount() {
  return renderRoutes(
    [
      { path: '/', element: <RestartButton pollMs={10} giveUpMs={200} /> },
      { path: '/login', element: <p>login page</p> },
      { path: '/devices', element: <p>devices page</p> },
    ],
    '/',
  )
}

async function confirmRestart() {
  await userEvent.click(screen.getByRole('button', { name: 'Restart server' }))
  const dialog = await screen.findByRole('dialog')
  expect(dialog).toHaveTextContent('pins are lost')
  await userEvent.click(within(dialog).getByRole('button', { name: 'Restart' }))
}

describe('RestartButton', () => {
  it('restarts, waits for the new server, then opens the login', async () => {
    // Il vecchio risponde ancora, poi è spento, poi il nuovo dà 401.
    const f = server([up(OLD), up(OLD), down, unauthorized], accepted)
    mount()
    await confirmRestart()
    expect(await screen.findByText(/Restarting the server/)).toBeInTheDocument()
    expect(f.mock.calls.some(([url, init]) => url === '/api/v1/admin/restart' && init?.method === 'POST')).toBe(true)
    expect(await screen.findByText('login page')).toBeInTheDocument()
  })

  it('opens the devices when the new server has no token', async () => {
    server([up(OLD), up(OLD), up(NEW)], accepted)
    mount()
    await confirmRestart()
    expect(await screen.findByText('devices page')).toBeInTheDocument()
  })

  it('does not restart with a broken config, and says why', async () => {
    const f = server([up(OLD)], () => jsonResponse(400, { error: { code: 'invalid_config', message: 'config.json would not start the server: stream config: bad' } }))
    mount()
    await confirmRestart()
    expect(await screen.findByText('Not restarted')).toBeInTheDocument()
    expect(screen.getByText(/would not start the server: stream config: bad/)).toBeInTheDocument()
    expect(screen.queryByText(/Restarting the server/)).not.toBeInTheDocument()
    await new Promise((r) => setTimeout(r, 50))
    expect(f).toHaveBeenCalledTimes(2) // status + restart: nessun polling
  })

  it('does nothing when the confirmation is cancelled', async () => {
    const f = server([up(OLD)], accepted)
    mount()
    await userEvent.click(screen.getByRole('button', { name: 'Restart server' }))
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Cancel' }))
    expect(f).not.toHaveBeenCalled()
  })

  it('goes to the login when the session has expired', async () => {
    server([unauthorized], accepted)
    mount()
    await confirmRestart()
    expect(await screen.findByText('login page')).toBeInTheDocument()
  })

  it('says when the server does not come back', async () => {
    server([up(OLD), down], accepted)
    mount()
    await confirmRestart()
    expect(await screen.findByText('The server did not come back', {}, { timeout: 2000 })).toBeInTheDocument()
  })
})
