import { useEffect, useState } from 'react'

// useNow restituisce l'ora attuale (ms) e fa ridisegnare ogni periodMs: per
// le età ("3 s ago") che devono crescere anche senza eventi nuovi.
//
// shortcut: l'età usa l'orologio del browser contro last_seen del server; con
// un server remoto senza NTP (scarto di orologio) è sbagliata dello scarto.
// Se serve, calcolarla sull'uptime del server.
export function useNow(periodMs = 1000): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), periodMs)
    return () => clearInterval(id)
  }, [periodMs])
  return now
}
