import type { ConfigFile, ConfigSaveResponse, ErrorBody, LoginResponse, ServerStatus, SetPolicyRequest, SetPolicyResponse } from './types'

// Le chiamate alla porta admin. Percorsi relativi all'origine: in
// produzione la pagina e l'API sono sullo stesso server Go, in sviluppo il
// proxy di Vite le inoltra (vite.config.ts). fetch manda il cookie di
// sessione da sé (credentials "same-origin" è il default).

export const EVENTS_URL = '/api/v1/admin/events'

// ApiError è una risposta non 2xx. code è il code JSON del server
// ("unauthorized", "invalid_request", …): si decide su quello, non sul
// messaggio. Senza corpo JSON (il 403 di CrossOriginProtection è testo)
// code è "http_<status>".
export class ApiError extends Error {
  readonly status: number
  readonly code: string

  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

export function isUnauthorized(err: unknown): boolean {
  return err instanceof ApiError && err.status === 401
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method }
  if (body !== undefined) {
    init.headers = { 'Content-Type': 'application/json' }
    init.body = JSON.stringify(body)
  }
  const res = await fetch(path, init)
  if (!res.ok) {
    let code = `http_${res.status}`
    let message = res.statusText
    try {
      const e = (await res.json()) as ErrorBody
      code = e.error.code
      message = e.error.message
    } catch {
      // corpo non JSON: restano status e statusText
    }
    throw new ApiError(res.status, code, message)
  }
  if (res.status === 204) {
    return undefined as T
  }
  return (await res.json()) as T
}

export const api = {
  login: (token: string, name: string) =>
    request<LoginResponse>('POST', '/api/v1/admin/login', { token, name }),
  logout: () => request<void>('POST', '/api/v1/admin/logout'),
  status: () => request<ServerStatus>('GET', '/api/v1/admin/status'),
  // pin: impone la policy; l'algoritmo non tocca più il device (OFFLOAD.md#admin).
  pin: (deviceID: string, p: SetPolicyRequest) =>
    request<SetPolicyResponse>('POST', policyPath(deviceID), p),
  // unpin: restituisce il device all'algoritmo; la policy resta finché
  // l'algoritmo non decide altrimenti.
  unpin: (deviceID: string) => request<SetPolicyResponse>('DELETE', policyPath(deviceID)),
  // config.json: leggere, validare (stessi controlli dell'avvio), salvare.
  // Il salvataggio vale dal prossimo avvio del server.
  getConfig: () => request<ConfigFile>('GET', '/api/v1/admin/config'),
  validateConfig: (content: string) => request<{ valid: boolean }>('POST', '/api/v1/admin/config/validate', { content }),
  saveConfig: (content: string, baseHash: string) =>
    request<ConfigSaveResponse>('PUT', '/api/v1/admin/config', { content, base_hash: baseHash }),
  // restart: il server controlla config.json, risponde 202 e si riavvia
  // (stesso processo, stessi argomenti). 400 invalid_config: non riparte.
  restart: () => request<{ restarting: boolean }>('POST', '/api/v1/admin/restart'),
}

function policyPath(deviceID: string): string {
  return `/api/v1/admin/devices/${encodeURIComponent(deviceID)}/policy`
}
