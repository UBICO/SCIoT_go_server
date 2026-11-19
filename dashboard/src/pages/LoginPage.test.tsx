import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { jsonResponse, renderRoutes } from '../test/fixtures'
import { LoginPage } from './LoginPage'

afterEach(() => vi.unstubAllGlobals())

function mount() {
  return renderRoutes(
    [
      { path: '/login', Component: LoginPage },
      { path: '/devices', element: <p>devices page</p> },
    ],
    '/login',
  )
}

async function fill(name: string, token: string) {
  const user = userEvent.setup()
  await user.type(screen.getByLabelText(/^Name/), name)
  await user.type(screen.getByLabelText(/^Admin token/), token)
  await user.click(screen.getByRole('button', { name: 'Log in' }))
}

describe('LoginPage', () => {
  it('logs in and opens the devices', async () => {
    const f = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse(200, { name: 'francesco', expires_at: 'x' }))
    vi.stubGlobal('fetch', f)
    mount()
    await fill('  francesco ', 'secret-token-123456')
    expect(await screen.findByText('devices page')).toBeInTheDocument()
    expect(JSON.parse(f.mock.calls[0]![1]!.body as string)).toEqual({ token: 'secret-token-123456', name: 'francesco' })
  })

  it('says when the token is wrong', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(401, { error: { code: 'unauthorized', message: 'x' } })))
    mount()
    await fill('francesco', 'wrong')
    expect(await screen.findByText('Wrong token.')).toBeInTheDocument()
  })

  it('says when the server does not answer', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
    mount()
    await fill('francesco', 'x')
    expect(await screen.findByText('The server does not answer.')).toBeInTheDocument()
  })

  it('needs a name', () => {
    mount()
    expect(screen.getByRole('button', { name: 'Log in' })).toBeDisabled()
  })
})
