import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { decision, device, liveState, renderLive } from '../test/fixtures'
import { DeviceListPage } from './DeviceListPage'

describe('DeviceListPage', () => {
  it('waits for the first snapshot', () => {
    renderLive(<DeviceListPage />, liveState([], null))
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })

  it('says when no device is registered', () => {
    renderLive(<DeviceListPage />, liveState([]))
    expect(screen.getByText('No device registered.')).toBeInTheDocument()
  })

  it('lists the devices sorted, with policy and last decision', () => {
    renderLive(
      <DeviceListPage />,
      liveState([
        device({ device_id: 'pi_b', pinned: true, last_decision: decision('2026-11-19T12:00:00Z') }),
        device({ device_id: 'pi_a', policy: { mode: 'LOCAL', split_point: null, policy_version: 1 }, applied_policy_version: 0, telemetry: null }),
      ]),
    )
    const rows = screen.getAllByRole('row').slice(1) // senza l'intestazione
    expect(rows.map((r) => within(r).getAllByRole('cell')[0]?.textContent)).toEqual(['pi_a', 'pi_b'])

    const [a, b] = rows as [HTMLElement, HTMLElement]
    expect(within(a).getByText('LOCAL')).toBeInTheDocument()
    expect(within(a).getAllByText('–').length).toBeGreaterThan(0) // nessun heartbeat
    expect(within(b).getByText('SPLIT @5 ×2')).toBeInTheDocument()
    expect(within(b).getByText('pinned')).toBeInTheDocument()
    expect(within(b).getByText('40.5 ms')).toBeInTheDocument()
    expect(within(b).getByText('split 5 is faster')).toBeInTheDocument()
  })

  it('marks a policy the device does not apply yet', () => {
    renderLive(<DeviceListPage />, liveState([device({ applied_policy_version: 1 })]))
    expect(screen.getByText('pending')).toBeInTheDocument()
  })

  it('opens a device with a click on the row', async () => {
    renderLive(<DeviceListPage />, liveState([device({ device_id: 'pi a/1' })]))
    await userEvent.click(screen.getByText('python'))
    expect(screen.getByText('elsewhere')).toBeInTheDocument()
  })

  it('links the device id', () => {
    renderLive(<DeviceListPage />, liveState([device({ device_id: 'pi a/1' })]))
    expect(screen.getByRole('link', { name: 'pi a/1' })).toHaveAttribute('href', '/devices/pi%20a%2F1')
  })
})
