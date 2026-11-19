import { createContext, useContext } from 'react'
import type { LiveState } from './state'

// Connection è lo stato dello stream:
//   - connecting: aperto ma non ancora connesso, o il browser sta riprovando
//     (server fermo, rete giù: EventSource riprova da sé);
//   - open: gli eventi arrivano;
//   - closed: il server ha risposto con un errore (EventSource non riprova);
//     LiveProvider controlla il login e riapre dopo qualche secondo.
export type Connection = 'connecting' | 'open' | 'closed'

export interface LiveValue {
  state: LiveState
  connection: Connection
}

export const LiveContext = createContext<LiveValue | null>(null)

export function useLive(): LiveValue {
  const v = useContext(LiveContext)
  if (v === null) {
    throw new Error('useLive fuori da <LiveProvider>')
  }
  return v
}
