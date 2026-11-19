import { notifications } from '@mantine/notifications'
import { act, fireEvent, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ConfigFile } from '../api/types'
import { configLoader } from '../router'
import { jsonResponse, renderRoutes } from '../test/fixtures'
import { ConfigPage } from './ConfigPage'

afterEach(() => {
  vi.unstubAllGlobals()
  act(() => notifications.clean())
})

const V1 = '{\n  "server": {},\n  "models": []\n}\n'
const V2 = '{\n  "server": {"heartbeat_interval_ms": 500},\n  "models": []\n}\n'

function file(over: Partial<ConfigFile> = {}): ConfigFile {
  return { path: '/srv/sciot/config.json', content: V1, hash: 'h1', modified_since_start: false, ...over }
}

type Handler = (method: string, body: Record<string, unknown> | undefined) => Response

// server: un fetch finto con un handler per ogni percorso; ricorda le
// chiamate.
function server(routes: Record<string, Handler>) {
  const f = vi.fn<typeof fetch>().mockImplementation(async (input, init) => {
    const url = String(input)
    const h = routes[url]
    if (h === undefined) {
      throw new Error(`unexpected ${url}`)
    }
    return h(init?.method ?? 'GET', init?.body === undefined ? undefined : JSON.parse(init.body as string))
  })
  vi.stubGlobal('fetch', f)
  return f
}

function mount() {
  return renderRoutes(
    [
      { path: '/config', Component: ConfigPage, loader: configLoader },
      { path: '/login', element: <p>login page</p> },
    ],
    '/config',
  )
}

// line: una riga del diff, testo esatto (il matcher di default normalizza
// gli spazi e non trova le righe con l'indentazione).
const line = (text: string) => (_: string, el: Element | null) => el?.tagName === 'DIV' && el.textContent === text

// edit: sostituisce il testo dell'editor (userEvent.type interpreta "{"
// come un tasto speciale: si usa un evento di change).
async function edit(text: string) {
  const editor = await screen.findByLabelText('config.json')
  fireEvent.change(editor, { target: { value: text } })
}

describe('ConfigPage', () => {
  it('shows the file and its path', async () => {
    server({ '/api/v1/admin/config': () => jsonResponse(200, file()) })
    mount()
    expect(await screen.findByLabelText('config.json')).toHaveValue(V1)
    expect(screen.getByText('/srv/sciot/config.json')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled() // nulla da salvare
    expect(screen.queryByText('Changes')).not.toBeInTheDocument()
  })

  it('says when the server runs with another version', async () => {
    server({ '/api/v1/admin/config': () => jsonResponse(200, file({ modified_since_start: true })) })
    mount()
    expect(await screen.findByText(/differs from the configuration the server started with/)).toBeInTheDocument()
  })

  it('says when config editing is not enabled', async () => {
    server({ '/api/v1/admin/config': () => jsonResponse(404, { error: { code: 'not_found', message: 'x' } }) })
    mount()
    expect(await screen.findByText('Config editing is not enabled on this server.')).toBeInTheDocument()
  })

  it('goes to the login without a session', async () => {
    server({ '/api/v1/admin/config': () => jsonResponse(401, { error: { code: 'unauthorized', message: 'x' } }) })
    mount()
    expect(await screen.findByText('login page')).toBeInTheDocument()
  })

  it('shows the diff of the edits, and discards them', async () => {
    server({ '/api/v1/admin/config': () => jsonResponse(200, file()) })
    mount()
    await edit(V2)
    expect(screen.getByText(line('-  "server": {},'))).toBeInTheDocument()
    expect(screen.getByText(line('+  "server": {"heartbeat_interval_ms": 500},'))).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled()

    await userEvent.click(screen.getByRole('button', { name: 'Discard changes' }))
    expect(screen.getByLabelText('config.json')).toHaveValue(V1)
    expect(screen.queryByText('Changes')).not.toBeInTheDocument()
  })

  it('validates with the server and shows its message', async () => {
    let valid = true
    const f = server({
      '/api/v1/admin/config': () => jsonResponse(200, file()),
      '/api/v1/admin/config/validate': () => (valid ? jsonResponse(200, { valid: true }) : jsonResponse(400, { error: { code: 'invalid_config', message: 'stream config: stream.max_inflight must be >= 1, got 0' } })),
    })
    mount()
    await edit(V2)
    await userEvent.click(screen.getByRole('button', { name: 'Validate' }))
    expect(await screen.findByText('Valid: the server would start with this configuration.')).toBeInTheDocument()
    expect(f.mock.calls.at(-1)?.[1]).toMatchObject({ method: 'POST', body: JSON.stringify({ content: V2 }) })

    valid = false
    await userEvent.click(screen.getByRole('button', { name: 'Validate' }))
    expect(await screen.findByText('stream config: stream.max_inflight must be >= 1, got 0')).toBeInTheDocument()

    // Un'altra modifica: l'esito non vale più.
    await edit(V2 + ' ')
    expect(screen.queryByText(/must be >= 1/)).not.toBeInTheDocument()
  })

  it('saves with the hash it loaded, after a confirmation, then reloads', async () => {
    let current = file()
    const f = server({
      '/api/v1/admin/config': (method, body) => {
        if (method === 'PUT') {
          current = file({ content: body?.content as string, hash: 'h2', modified_since_start: true })
          return jsonResponse(200, { changed: true, hash: 'h2', backup: 'config.json.20261119-150405.bak', restart_required: true })
        }
        return jsonResponse(200, current)
      },
    })
    mount()
    await edit(V2)
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(line('+  "server": {"heartbeat_interval_ms": 500},'))).toBeInTheDocument()
    expect(f.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(false) // niente prima della conferma
    await userEvent.click(within(dialog).getByRole('button', { name: 'Save' }))

    const put = f.mock.calls.find(([, init]) => init?.method === 'PUT')
    expect(JSON.parse(put?.[1]?.body as string)).toEqual({ content: V2, base_hash: 'h1' })
    expect(await screen.findByText(/restart it to apply the new one/)).toBeInTheDocument()
    expect(screen.getByText(/The previous file is in config\.json\.20261119-150405\.bak/)).toBeInTheDocument()
    expect(screen.getByText('Backup: config.json.20261119-150405.bak')).toBeInTheDocument() // la notifica
    // Ricaricato: il file salvato è la nuova base, niente più da salvare.
    expect(screen.getByLabelText('config.json')).toHaveValue(V2)
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled()
  })

  it('shows a refused save under the editor', async () => {
    server({
      '/api/v1/admin/config': (method) =>
        method === 'PUT' ? jsonResponse(400, { error: { code: 'invalid_config', message: 'worker.command cannot be changed from the dashboard: edit config.json on the server' } }) : jsonResponse(200, file()),
    })
    mount()
    await edit(V2)
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save' }))
    expect(await screen.findByText(/worker.command cannot be changed/)).toBeInTheDocument()
    expect(screen.getByLabelText('config.json')).toHaveValue(V2) // le modifiche restano
  })

  it('stops on a conflict and reloads only when asked', async () => {
    let current = file()
    server({
      '/api/v1/admin/config': (method) => {
        if (method === 'PUT') {
          current = file({ content: '{"changed": "by hand"}\n', hash: 'h3' })
          return jsonResponse(409, { error: { code: 'config_changed', message: 'x' } })
        }
        return jsonResponse(200, current)
      },
    })
    mount()
    await edit(V2)
    await userEvent.click(screen.getByRole('button', { name: 'Save' }))
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Save' }))

    expect(await screen.findByText('The file changed on the server')).toBeInTheDocument()
    expect(screen.getByLabelText('config.json')).toHaveValue(V2) // le tue modifiche, da copiare
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled()

    await userEvent.click(screen.getByRole('button', { name: /Reload the file/ }))
    expect(await screen.findByDisplayValue('{"changed": "by hand"}')).toBeInTheDocument()
    expect(screen.queryByText('The file changed on the server')).not.toBeInTheDocument()
  })
})
