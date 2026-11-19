import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { decision, device, liveState, renderLive } from '../test/fixtures'
import { liveReducer } from '../live/state'
import { DeviceDetailPage } from './DeviceDetailPage'

function mount(state: ReturnType<typeof liveState>, id = 'pi_a') {
  return renderLive(<DeviceDetailPage />, state, { path: `/devices/${id}`, pattern: '/devices/:id' })
}

describe('DeviceDetailPage', () => {
  it('says when the device is not registered', () => {
    mount(liveState([]), 'ghost')
    expect(screen.getByText(/is not registered/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'All devices' })).toHaveAttribute('href', '/devices')
  })

  it('shows the last heartbeat and the measured network', () => {
    mount(liveState([device()]))
    expect(screen.getByRole('heading', { name: 'pi_a' })).toBeInTheDocument()
    expect(screen.getByText('40.5 ms / 61.2 ms')).toBeInTheDocument()
    expect(screen.getByText('42 %')).toBeInTheDocument()
    expect(screen.getByText('11.00 MB/s')).toBeInTheDocument()
    expect(screen.getByText('python · yolo_pico_320 · session FK2FWZX4')).toBeInTheDocument()
  })

  it('draws the four charts', () => {
    mount(liveState([device()]))
    for (const title of ['Latency (ms)', 'Policy (server)', 'Device CPU (%)', 'Bandwidth measured by the server (MB/s)']) {
      expect(screen.getByText(title)).toBeInTheDocument()
    }
  })

  it('says when a chart has no value at all', () => {
    mount(liveState([device({ network: null })]))
    expect(screen.getAllByText('No value in the heartbeats so far.')).toHaveLength(1) // solo la banda
  })

  it('says that a pinned device is left alone by the algorithm', () => {
    mount(liveState([device({ pinned: true, last_decision: decision('2026-11-19T12:00:00Z') })]))
    expect(screen.getByText(/Pinned by the admin since/)).toBeInTheDocument()
  })

  it('says that the static algorithm never decides', () => {
    mount(liveState([device()]))
    expect(screen.getByText(/No decision yet/)).toBeInTheDocument()
    expect(screen.queryByText('Policy changes')).not.toBeInTheDocument()
  })

  it('shows the last decision with its predictions, the chosen split in bold', () => {
    mount(liveState([device({ last_decision: decision('2026-11-19T12:00:00Z') })]))
    expect(screen.getByText(/Last decision: LOCAL → SPLIT @5 ×2/)).toBeInTheDocument()
    expect(screen.getAllByText('split 5 is faster')).toHaveLength(2) // ultima decisione + cambi

    const table = screen.getAllByRole('table')[0]!
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows).toHaveLength(2)
    const [local, split] = rows as [HTMLElement, HTMLElement]
    expect(within(local).getByText('LOCAL')).toBeInTheDocument()
    expect(within(split).getByText('@5 ×2')).toBeInTheDocument()
    expect(within(split).getByText('70.0 ms')).toBeInTheDocument() // p95 prevista
    expect(split).toHaveStyle({ fontWeight: '700' })
    expect(local).not.toHaveStyle({ fontWeight: '700' })
  })

  it('lists the policy changes, newest first, without the decisions that keep it', () => {
    let s = liveState([device({ last_decision: decision('2026-11-19T12:00:00Z') })])
    s = liveReducer(s, { type: 'device', device: device({ last_decision: decision('2026-11-19T12:00:03Z', { changed: false, reason: 'keep' }) }) })
    s = liveReducer(s, { type: 'device', device: device({ last_decision: decision('2026-11-19T12:00:05Z', { current: { mode: 'SPLIT', split_point: 5, policy_version: 2 }, mode: 'LOCAL', split_point: null, reason: 'local is faster' }) }) })
    mount(s)
    const history = screen.getByText('Policy changes').parentElement!
    const rows = within(history).getAllByRole('row').slice(1).map((r) => within(r).getAllByRole('cell').slice(1).map((c) => c.textContent))
    expect(rows).toEqual([
      ['SPLIT @5 → LOCAL', 'local is faster'],
      ['LOCAL → SPLIT @5 ×2', 'split 5 is faster'],
    ])
  })
})
