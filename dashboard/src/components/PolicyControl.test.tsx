import { notifications } from '@mantine/notifications'
import { act, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { AdminDevice } from '../api/types'
import { device, jsonResponse, renderRoutes } from '../test/fixtures'
import { PolicyControl } from './PolicyControl'

afterEach(() => {
  vi.unstubAllGlobals()
  act(() => notifications.clean())
})

function mount(d: AdminDevice) {
  return renderRoutes(
    [
      { path: '/', element: <PolicyControl device={d} /> },
      { path: '/login', element: <p>login page</p> },
    ],
    '/',
  )
}

function stubFetch(res: () => Response) {
  const f = vi.fn<typeof fetch>().mockImplementation(async () => res())
  vi.stubGlobal('fetch', f)
  return f
}

const okPolicy = (policy: object) => () => jsonResponse(200, { device_id: 'pi_a', session_id: 'S', policy })

// confirm: apre la conferma con il pulsante e conferma.
async function pinAndConfirm(button = 'Pin') {
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: button }))
  const dialog = await screen.findByRole('dialog')
  await user.click(within(dialog).getByRole('button', { name: 'Confirm' }))
  return dialog
}

describe('PolicyControl', () => {
  it('offers the valid splits of the model and the windows of the stream', () => {
    mount(device({ last_offloading_layer: 7, max_inflight_max: 3 }))
    const splits = within(screen.getByLabelText('Split point')).getAllByRole('option')
    expect(splits.map((o) => o.getAttribute('value'))).toEqual(['-1', '0', '1', '2', '3', '4', '5', '6'])
    expect(splits[0]).toHaveTextContent('@-1 (raw input)')
    const windows = within(screen.getByLabelText('Window')).getAllByRole('option')
    expect(windows.map((o) => o.textContent)).toEqual(['max (3)', '1', '2', '3'])
  })

  it('starts from the current policy', () => {
    mount(device({ policy: { mode: 'SPLIT', split_point: 5, policy_version: 2, max_inflight: 2 } }))
    expect(screen.getByLabelText('Split point')).toHaveValue('5')
    expect(screen.getByLabelText('Window')).toHaveValue('2')
  })

  it('has no window without data plane', () => {
    mount(device({ max_inflight_max: null }))
    expect(screen.queryByLabelText('Window')).not.toBeInTheDocument()
  })

  it('pins the chosen split and window after a confirmation', async () => {
    const f = stubFetch(okPolicy({ mode: 'SPLIT', split_point: 3, policy_version: 3, max_inflight: 1 }))
    mount(device({ device_id: 'pi a' }))
    await userEvent.selectOptions(screen.getByLabelText('Split point'), '3')
    await userEvent.selectOptions(screen.getByLabelText('Window'), '1')

    await userEvent.click(screen.getByRole('button', { name: 'Pin' }))
    const dialog = await screen.findByRole('dialog')
    expect(dialog).toHaveTextContent('Pin pi a to SPLIT @3 ×1?')
    expect(f).not.toHaveBeenCalled() // niente prima della conferma
    await userEvent.click(within(dialog).getByRole('button', { name: 'Confirm' }))

    const [url, init] = f.mock.calls[0]!
    expect(url).toBe('/api/v1/admin/devices/pi%20a/policy')
    expect(init?.method).toBe('POST')
    expect(JSON.parse(init?.body as string)).toEqual({ mode: 'SPLIT', split_point: 3, max_inflight: 1 })
    expect(await screen.findByText('pi_a: SPLIT @3 ×1 (v3)')).toBeInTheDocument()
  })

  it('pins LOCAL with a null split and no window', async () => {
    const f = stubFetch(okPolicy({ mode: 'LOCAL', split_point: null, policy_version: 3 }))
    mount(device())
    await userEvent.click(screen.getByRole('radio', { name: 'LOCAL' }))
    expect(screen.getByLabelText('Split point')).toBeDisabled()
    await pinAndConfirm()
    expect(JSON.parse(f.mock.calls[0]![1]!.body as string)).toEqual({ mode: 'LOCAL', split_point: null })
  })

  it('sends no window when the maximum is chosen', async () => {
    const f = stubFetch(okPolicy({ mode: 'SPLIT', split_point: 5, policy_version: 3 }))
    mount(device({ policy: { mode: 'SPLIT', split_point: 5, policy_version: 2 } }))
    await pinAndConfirm()
    expect(JSON.parse(f.mock.calls[0]![1]!.body as string)).toEqual({ mode: 'SPLIT', split_point: 5 })
  })

  it('does nothing when the confirmation is cancelled', async () => {
    const f = stubFetch(okPolicy({}))
    mount(device())
    await userEvent.click(screen.getByRole('button', { name: 'Pin' }))
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Cancel' }))
    expect(f).not.toHaveBeenCalled()
  })

  it('unpins a pinned device', async () => {
    const f = stubFetch(okPolicy({ mode: 'SPLIT', split_point: 5, policy_version: 2 }))
    mount(device({ pinned: true }))
    const dialog = await pinAndConfirm('Unpin')
    expect(dialog).toHaveTextContent('Its policy stays SPLIT @5 ×2 until the algorithm decides otherwise.')
    const [url, init] = f.mock.calls[0]!
    expect(url).toBe('/api/v1/admin/devices/pi_a/policy')
    expect(init?.method).toBe('DELETE')
    expect(await screen.findByText('Unpinned')).toBeInTheDocument()
  })

  it('has no unpin for a device that is not pinned', () => {
    mount(device({ pinned: false }))
    expect(screen.queryByRole('button', { name: 'Unpin' })).not.toBeInTheDocument()
  })

  it('shows why the server refused the pin', async () => {
    stubFetch(() => jsonResponse(400, { error: { code: 'invalid_request', message: 'split_point 9 is out of range' } }))
    mount(device())
    await pinAndConfirm()
    expect(await screen.findByText('Pin refused')).toBeInTheDocument()
    expect(screen.getByText('split_point 9 is out of range')).toBeInTheDocument()
  })

  it('goes to the login when the session has expired', async () => {
    stubFetch(() => jsonResponse(401, { error: { code: 'unauthorized', message: 'x' } }))
    mount(device())
    await pinAndConfirm()
    expect(await screen.findByText('login page')).toBeInTheDocument()
  })
})
