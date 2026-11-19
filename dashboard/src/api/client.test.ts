import { afterEach, describe, expect, it, vi } from 'vitest'
import { jsonResponse, status } from '../test/fixtures'
import { api, ApiError, isUnauthorized } from './client'

afterEach(() => {
  vi.unstubAllGlobals()
})

function stubFetch(res: Response) {
  const f = vi.fn<typeof fetch>().mockResolvedValue(res)
  vi.stubGlobal('fetch', f)
  return f
}

describe('api', () => {
  it('logs in with a JSON body', async () => {
    const f = stubFetch(jsonResponse(200, { name: 'francesco', expires_at: '2026-11-20T00:00:00Z' }))
    await expect(api.login('tok', 'francesco')).resolves.toEqual({ name: 'francesco', expires_at: '2026-11-20T00:00:00Z' })
    const [url, init] = f.mock.calls[0]!
    expect(url).toBe('/api/v1/admin/login')
    expect(init?.method).toBe('POST')
    expect(init?.headers).toEqual({ 'Content-Type': 'application/json' })
    expect(JSON.parse(init?.body as string)).toEqual({ token: 'tok', name: 'francesco' })
  })

  it('reads the status', async () => {
    stubFetch(jsonResponse(200, status()))
    await expect(api.status()).resolves.toEqual(status())
  })

  it('logs out with no body and accepts 204', async () => {
    const f = stubFetch(jsonResponse(204))
    await expect(api.logout()).resolves.toBeUndefined()
    expect(f.mock.calls[0]?.[1]?.body).toBeUndefined()
  })

  it('turns a JSON error into an ApiError with its code', async () => {
    stubFetch(jsonResponse(401, { error: { code: 'unauthorized', message: 'missing or invalid admin credentials' } }))
    const err = await api.status().catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({ status: 401, code: 'unauthorized', message: 'missing or invalid admin credentials' })
    expect(isUnauthorized(err)).toBe(true)
  })

  it('handles a non-JSON error (CrossOriginProtection)', async () => {
    stubFetch(new Response('cross-origin request detected', { status: 403, statusText: 'Forbidden' }))
    const err = await api.logout().catch((e: unknown) => e)
    expect(err).toMatchObject({ status: 403, code: 'http_403' })
    expect(isUnauthorized(err)).toBe(false)
  })
})
