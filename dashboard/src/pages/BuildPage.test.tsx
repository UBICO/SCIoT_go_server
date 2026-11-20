import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { api } from '../api/client'
import type { BuildsResponse, BuildView, ConfigFile } from '../api/types'
import { BuildJob } from '../components/BuildJob'
import { withModel } from '../configEdit'
import { configLoader } from '../router'
import { jsonResponse, renderRoutes } from '../test/fixtures'
import { BuildPage } from './BuildPage'
import { ConfigPage } from './ConfigPage'
import { ModelsPage } from './ModelsPage'

afterEach(() => vi.unstubAllGlobals())

type Handler = (init: RequestInit | undefined, url: URL) => Response

// server: un fetch finto per percorso (senza query); ricorda le chiamate.
function server(routes: Record<string, Handler>) {
  const f = vi.fn<typeof fetch>().mockImplementation(async (input, init) => {
    const url = new URL(String(input), 'http://x')
    const h = routes[url.pathname]
    if (h === undefined) {
      throw new Error(`unexpected ${url.pathname}`)
    }
    return h(init, url)
  })
  vi.stubGlobal('fetch', f)
  return f
}

function job(over: Partial<BuildView> = {}): BuildView {
  return { id: 'yolo-1', model_key: 'yolo', output: 'models/yolo', who: 'francesco', state: 'running', started_at: '2026-11-20T10:00:00Z', log_lines: 0, ...over }
}

const enabled: BuildsResponse = { enabled: true, models_dir: 'models', builds: [] }

function mountBuild(path = '/models/build') {
  return renderRoutes(
    [
      { path: '/models/build', Component: BuildPage, loader: api.builds },
      { path: '/models/builds/:id', element: <p>job page</p> },
      { path: '/login', element: <p>login page</p> },
    ],
    path,
  )
}

// fileInputs: gli input nascosti dei FileInput, in ordine: SavedModel, C++, immagini.
function fileInputs(): HTMLInputElement[] {
  return Array.from(document.querySelectorAll('input[type="file"]'))
}

describe('BuildPage', () => {
  it('says how to turn builds on', async () => {
    server({ '/api/v1/admin/models/builds': () => jsonResponse(200, { enabled: false, builds: [] }) })
    mountBuild()
    expect(await screen.findByText(/set "models_dir" in config.json/)).toBeInTheDocument()
  })

  it('uploads the exports and the options, then opens the build', async () => {
    const f = server({
      '/api/v1/admin/models/builds': (init) => (init?.method === 'POST' ? jsonResponse(202, job()) : jsonResponse(200, enabled)),
    })
    mountBuild()
    const user = userEvent.setup()
    const submit = await screen.findByRole('button', { name: 'Upload and build' })
    expect(submit).toBeDisabled()

    await user.type(screen.getByLabelText(/Model key/), 'Bad Key')
    expect(screen.getByText(/lowercase letters, digits/)).toBeInTheDocument()
    await user.clear(screen.getByLabelText(/Model key/))
    await user.type(screen.getByLabelText(/Model key/), 'yolo_small_320')

    const [sm, cpp, samples] = fileInputs() as [HTMLInputElement, HTMLInputElement, HTMLInputElement]
    await user.upload(sm, new File(['SM'], 'savedmodel.41.zip', { type: 'application/zip' }))
    await user.upload(cpp, new File(['CPP'], 'cpp.zip', { type: 'application/zip' }))
    await user.upload(samples, [new File(['P'], 'stop.png', { type: 'image/png' }), new File(['J'], 'bike.jpg', { type: 'image/jpeg' })])
    await user.clear(screen.getByLabelText(/Max segments/))
    await user.type(screen.getByLabelText(/Max segments/), '3')
    // TagsInput ha più elementi legati all'etichetta: si scrive nell'input visibile.
    const cuts = screen.getAllByLabelText(/Cuts/).find((el) => el.tagName === 'INPUT' && el.getAttribute('type') !== 'hidden')!
    await user.type(cuts, 'model/conv_1/Relu{Enter}')

    await user.click(submit)
    expect(await screen.findByText('job page')).toBeInTheDocument()
    const post = f.mock.calls.find(([, init]) => init?.method === 'POST')
    const form = post?.[1]?.body as FormData
    expect(form).toBeInstanceOf(FormData)
    expect(form.get('model_key')).toBe('yolo_small_320')
    expect((form.get('savedmodel') as File).name).toBe('savedmodel.41.zip')
    expect((form.get('cpp_export') as File).name).toBe('cpp.zip')
    expect(form.getAll('sample').map((x) => (x as File).name)).toEqual(['stop.png', 'bike.jpg'])
    expect(form.get('max_segments')).toBe('3')
    expect(form.getAll('cut')).toEqual(['model/conv_1/Relu'])
    expect(post?.[1]?.headers).toBeUndefined() // il boundary lo mette il browser
  })

  it('shows why the server did not start the build', async () => {
    server({
      '/api/v1/admin/models/builds': (init) =>
        init?.method === 'POST' ? jsonResponse(409, { error: { code: 'model_exists', message: 'models/yolo already exists' } }) : jsonResponse(200, enabled),
    })
    mountBuild()
    const user = userEvent.setup()
    await user.type(await screen.findByLabelText(/Model key/), 'yolo')
    const [sm, cpp] = fileInputs() as [HTMLInputElement, HTMLInputElement]
    await user.upload(sm, new File(['SM'], 'a.zip'))
    await user.upload(cpp, new File(['C'], 'b.zip'))
    await user.click(screen.getByRole('button', { name: 'Upload and build' }))
    expect(await screen.findByText('models/yolo already exists')).toBeInTheDocument()
  })

  it('does not start a second build while one runs, and lists the recent ones', async () => {
    server({ '/api/v1/admin/models/builds': () => jsonResponse(200, { ...enabled, builds: [job(), job({ id: 'old-1', model_key: 'old', state: 'failed' })] }) })
    mountBuild()
    expect(await screen.findByText(/A build is running/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'yolo' })).toHaveAttribute('href', '/models/builds/yolo-1')
    expect(screen.getByRole('link', { name: 'old' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Upload and build' })).toBeDisabled()
  })
})

describe('BuildJob', () => {
  function mountJob(routes: Record<string, Handler>) {
    const f = server(routes)
    renderRoutes(
      [
        { path: '/', element: <BuildJob id="yolo-1" pollMs={10} /> },
        { path: '/config', Component: ConfigPage, loader: configLoader },
      ],
      '/',
    )
    return f
  }

  it('follows the log with since until the build ends, then offers to add the model', async () => {
    const steps = [
      job({ log: ['Reading the C++ export'], next: 1 }),
      job({ log: [], next: 1 }),
      job({ log: ['Selected 7 cuts', 'model_hash abc'], next: 3, log_lines: 3 }),
      job({ state: 'succeeded', exit_code: 0, model_hash: 'abc', log: [], next: 3, log_lines: 3 }),
    ]
    let i = 0
    const config: ConfigFile = { path: '/srv/config.json', content: '{"server": {}, "models": []}\n', hash: 'h', modified_since_start: false }
    const f = mountJob({
      '/api/v1/admin/models/builds/yolo-1': () => jsonResponse(200, steps[Math.min(i++, steps.length - 1)]),
      '/api/v1/admin/config': () => jsonResponse(200, config),
    })
    expect(await screen.findByText('Model built')).toBeInTheDocument()
    expect(screen.getByText(/Reading the C\+\+ export\s+Selected 7 cuts\s+model_hash abc/)).toBeInTheDocument()
    const sinces = f.mock.calls.filter(([u]) => String(u).includes('/builds/')).map(([u]) => new URL(String(u), 'http://x').searchParams.get('since'))
    expect(sinces).toEqual(['0', '1', '1', '3'])

    await userEvent.click(screen.getByRole('button', { name: 'Add to config.json' }))
    expect(await screen.findByText('Add yolo')).toBeInTheDocument()
    expect(screen.getByLabelText('config.json')).toHaveValue((withModel(config.content, { model_key: 'yolo', tflite_dir: 'models/yolo' }) as { text: string }).text)
  })

  it('shows a failed build and stops polling', async () => {
    const f = mountJob({ '/api/v1/admin/models/builds/yolo-1': () => jsonResponse(200, job({ state: 'failed', exit_code: 2, log: ['error: no single-tensor cut found'], next: 1 })) })
    expect(await screen.findByText('Build failed')).toBeInTheDocument()
    expect(screen.getByText('error: no single-tensor cut found')).toBeInTheDocument()
    await new Promise((r) => setTimeout(r, 50))
    expect(f).toHaveBeenCalledTimes(1)
  })

  it('cancels a running build', async () => {
    let cancelled = false
    const f = mountJob({
      '/api/v1/admin/models/builds/yolo-1': (init) => {
        if (init?.method === 'DELETE') {
          cancelled = true
          return jsonResponse(202, job())
        }
        return jsonResponse(200, cancelled ? job({ state: 'cancelled', log: [], next: 0 }) : job({ log: [], next: 0 }))
      },
    })
    await userEvent.click(await screen.findByRole('button', { name: 'Cancel the build' }))
    expect(await screen.findByText('cancelled')).toBeInTheDocument()
    expect(f.mock.calls.some(([, init]) => init?.method === 'DELETE')).toBe(true)
  })
})

describe('withModel', () => {
  it('adds the model and reformats', () => {
    const r = withModel('{"server": {}, "models": [{"model_key": "a", "tflite_dir": "models/a"}]}', { model_key: 'b', tflite_dir: 'models/b' })
    expect('text' in r && JSON.parse(r.text).models).toEqual([
      { model_key: 'a', tflite_dir: 'models/a' },
      { model_key: 'b', tflite_dir: 'models/b' },
    ])
    expect('text' in r && r.text.endsWith('}\n')).toBe(true)
  })

  it('says why it cannot', () => {
    const m = { model_key: 'a', tflite_dir: 'models/a' }
    expect(withModel('{oops', m)).toEqual({ reason: 'config.json is not valid JSON' })
    expect(withModel('{"server": {}}', m)).toEqual({ reason: 'config.json has no "models" list' })
    expect(withModel('{"models": [{"model_key": "a"}]}', m)).toEqual({ reason: '"a" is already in "models"' })
  })
})

describe('ModelsPage', () => {
  it('links the build page when builds are on', async () => {
    server({ '/x': () => jsonResponse(200, {}) })
    renderRoutes([{ path: '/', Component: ModelsPage, loader: () => ({ models: [], builds: enabled }) }], '/')
    expect(await screen.findByRole('link', { name: 'Build a model' })).toHaveAttribute('href', '/models/build')
  })

  it('has no build button when builds are off', async () => {
    renderRoutes([{ path: '/', Component: ModelsPage, loader: () => ({ models: [], builds: { enabled: false, builds: [] } }) }], '/')
    await screen.findByText('No model in the registry.')
    expect(screen.queryByRole('link', { name: 'Build a model' })).not.toBeInTheDocument()
    expect(within(document.body).queryByText('Build a model')).not.toBeInTheDocument()
  })
})
