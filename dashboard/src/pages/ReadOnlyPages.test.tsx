import { screen, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { api } from '../api/client'
import type { AdminModel, ChangeLogResponse, RecordingsResponse } from '../api/types'
import { jsonResponse, renderRoutes } from '../test/fixtures'
import { ChangeLogPage } from './ChangeLogPage'
import { ModelsPage } from './ModelsPage'
import { RecordingsPage } from './RecordingsPage'

// Le tre pagine in sola lettura: registrazioni, modelli, registro.

afterEach(() => vi.unstubAllGlobals())

function serve(body: unknown) {
  vi.stubGlobal('fetch', vi.fn().mockImplementation(async () => jsonResponse(200, body)))
}

describe('RecordingsPage', () => {
  function mount() {
    return renderRoutes([{ path: '/', Component: RecordingsPage, loader: api.recordings }], '/')
  }

  it('lists the files with a download link and marks the one being written', async () => {
    const data: RecordingsResponse = {
      recording: true,
      dir: '/srv/records',
      files: [
        { name: 'sciot-20261120-090000.jsonl', size_bytes: 2_500_000, modified: '2026-11-20T09:10:00Z', current: true },
        { name: 'sciot-20261119-100000.jsonl', size_bytes: 800, modified: '2026-11-19T11:00:00Z', current: false },
      ],
    }
    serve(data)
    mount()
    const link = await screen.findByRole('link', { name: 'sciot-20261120-090000.jsonl' })
    expect(link).toHaveAttribute('href', '/api/v1/admin/recordings/sciot-20261120-090000.jsonl')
    expect(link).toHaveAttribute('download', 'sciot-20261120-090000.jsonl')
    const rows = screen.getAllByRole('row').slice(1)
    expect(within(rows[0]!).getByText('recording')).toBeInTheDocument()
    expect(within(rows[0]!).getByText('2.4 MiB')).toBeInTheDocument()
    expect(within(rows[1]!).queryByText('recording')).not.toBeInTheDocument()
    expect(within(rows[1]!).getByText('800 B')).toBeInTheDocument()
    expect(screen.getByText(/\/srv\/records/)).toBeInTheDocument()
  })

  it('says when recording is off', async () => {
    serve({ recording: false, files: [] })
    mount()
    expect(await screen.findByText(/Recording is off/)).toBeInTheDocument()
  })

  it('says when there is no file yet', async () => {
    serve({ recording: true, dir: '/srv/records', files: [] })
    mount()
    expect(await screen.findByText('No recording yet.')).toBeInTheDocument()
  })
})

describe('ModelsPage', () => {
  it('shows each model with its devices', async () => {
    const m: AdminModel = {
      model_hash: 'abc123',
      model_key: 'yolo_pico_320',
      num_layers: 8,
      last_offloading_layer: 7,
      input: { shape: [1, 320, 320, 3], dtype: 'float32' },
      postprocess: { decoder: 'yolo_pro', labels: ['bicycle', 'stop sign'], score_threshold: 0.5, iou_threshold: 0.45, max_detections: 100 },
      devices: 2,
    }
    serve({ models: [m, { ...m, model_hash: 'def', model_key: 'fomo_96', devices: 0, postprocess: { ...m.postprocess, decoder: 'fomo', iou_threshold: undefined } }] })
    renderRoutes([{ path: '/', Component: ModelsPage, loader: api.models }], '/')
    expect(await screen.findByText('yolo_pico_320')).toBeInTheDocument()
    expect(screen.getByText('2 devices')).toBeInTheDocument()
    expect(screen.getByText('0 devices')).toBeInTheDocument()
    expect(screen.getByText('abc123')).toBeInTheDocument()
    expect(screen.getAllByText('8 · splits -1…6')).toHaveLength(2)
    expect(screen.getAllByText('1 × 320 × 320 × 3 float32')).toHaveLength(2)
    expect(screen.getByText('yolo_pro · score ≥ 0.5 · IoU 0.45 · max 100')).toBeInTheDocument()
    expect(screen.getByText('fomo · score ≥ 0.5 · max 100')).toBeInTheDocument()
    expect(screen.getAllByText('bicycle, stop sign')).toHaveLength(2)
  })
})

describe('ChangeLogPage', () => {
  function mount() {
    return renderRoutes([{ path: '/', Component: ChangeLogPage, loader: () => api.changelog() }], '/')
  }

  it('shows who did what, newest first', async () => {
    const data: ChangeLogResponse = {
      enabled: true,
      entries: [
        { ts: '2026-11-20T09:03:00Z', who: 'francesco', action: 'restart', config: { path: '/srv/config.json', hash: 'h' } },
        { ts: '2026-11-20T09:02:00Z', who: 'francesco', action: 'config', config: { path: '/srv/config.json', backup: 'config.json.20261120-100200.bak', hash: 'h' } },
        { ts: '2026-11-20T09:01:00Z', who: 'token', action: 'unpin', device_id: 'pi_a', policy: { mode: 'SPLIT', split_point: 3, policy_version: 2 }, pinned: false },
        { ts: '2026-11-20T09:00:00Z', who: 'giulia', action: 'pin', device_id: 'pi_a', policy: { mode: 'SPLIT', split_point: 3, policy_version: 2 }, pinned: true },
      ],
    }
    serve(data)
    mount()
    await screen.findByText('restart')
    const rows = screen.getAllByRole('row').slice(1)
    expect(rows.map((r) => within(r).getAllByRole('cell')[2]?.textContent)).toEqual(['restart', 'config', 'unpin', 'pin'])
    expect(within(rows[1]!).getByText('/srv/config.json (backup config.json.20261120-100200.bak)')).toBeInTheDocument()
    expect(within(rows[3]!).getByText('pi_a: SPLIT @3')).toBeInTheDocument()
    expect(within(rows[3]!).getByText('giulia')).toBeInTheDocument()
  })

  it('says when there is no change log', async () => {
    serve({ enabled: false, entries: [] })
    mount()
    expect(await screen.findByText(/add a "record" section/)).toBeInTheDocument()
  })

  it('asks for the last 200 lines', async () => {
    serve({ enabled: true, entries: [] })
    mount()
    expect(await screen.findByText('No change yet.')).toBeInTheDocument()
    expect(vi.mocked(fetch).mock.calls[0]?.[0]).toBe('/api/v1/admin/changelog?limit=200')
  })
})
