import { type ReactNode, useEffect, useReducer, useState } from 'react'
import { useNavigate } from 'react-router'
import { api, EVENTS_URL, isUnauthorized } from '../api/client'
import type { AdminDevice, ServerStatus } from '../api/types'
import { type Connection, LiveContext } from './context'
import { initialLiveState, liveReducer } from './state'

// Il minimo di EventSource che serve qui: i test passano un finto stream.
export interface EventStream {
  readyState: number
  onopen: ((ev: Event) => void) | null
  onerror: ((ev: Event) => void) | null
  addEventListener(type: string, listener: (ev: MessageEvent<string>) => void): void
  close(): void
}

export type OpenStream = (url: string) => EventStream

// A livello di modulo, non nella firma del componente: un default
// ricreato a ogni render farebbe ripartire l'effetto e riaprire lo stream.
const openEventSource: OpenStream = (url) => new EventSource(url)

// EventSource.CLOSED: costante scritta qui perché jsdom (i test) non ha
// EventSource.
const CLOSED = 2

// RETRY_MS: dopo un errore del server (stream chiuso), quanto aspettare
// prima di riaprire. EventSource da solo riprova solo se cade la
// connessione, non dopo una risposta di errore.
export const RETRY_MS = 3000

// LiveProvider apre lo stream degli eventi una volta per tutta l'area
// protetta (Layout) e lo condivide con le pagine via LiveContext: una
// connessione per scheda del browser, qualunque pagina sia aperta.
export function LiveProvider({ children, open = openEventSource }: { children: ReactNode; open?: OpenStream }) {
  const [state, dispatch] = useReducer(liveReducer, initialLiveState)
  const [connection, setConnection] = useState<Connection>('connecting')
  const [attempt, setAttempt] = useState(0) // cambia = riapri lo stream
  const navigate = useNavigate()

  useEffect(() => {
    const es = open(EVENTS_URL)
    let retry: ReturnType<typeof setTimeout> | undefined
    es.onopen = () => setConnection('open')
    es.addEventListener('status', (e) => dispatch({ type: 'status', status: JSON.parse(e.data) as ServerStatus }))
    es.addEventListener('device', (e) => dispatch({ type: 'device', device: JSON.parse(e.data) as AdminDevice }))
    es.onerror = () => {
      if (es.readyState !== CLOSED) {
        setConnection('connecting') // il browser riprova da sé
        return
      }
      // Risposta di errore: EventSource non dice quale. Se è la sessione
      // scaduta, GET /status risponde 401: si torna al login. Altrimenti si
      // riprova tra poco.
      setConnection('closed')
      const reopen = () => {
        retry = setTimeout(() => {
          setConnection('connecting')
          setAttempt((n) => n + 1)
        }, RETRY_MS)
      }
      api.status().then(reopen, (err: unknown) => {
        if (isUnauthorized(err)) {
          navigate('/login', { replace: true })
        } else {
          reopen()
        }
      })
    }
    return () => {
      clearTimeout(retry)
      es.close()
    }
  }, [open, navigate, attempt])

  return <LiveContext value={{ state, connection }}>{children}</LiveContext>
}
