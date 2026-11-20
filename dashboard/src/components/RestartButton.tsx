import { Alert, Button, Group, List, Loader, Modal, Stack, Text } from '@mantine/core'
import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router'
import { api, ApiError, isUnauthorized } from '../api/client'

// POLL_MS: ogni quanto chiedere GET /status mentre il server riparte.
export const POLL_MS = 1000
// GIVE_UP_MS: dopo quanto smettere di aspettare e dirlo.
export const GIVE_UP_MS = 60_000

// RestartButton: riavvia il server per applicare config.json
// (docs/DASHBOARD.md). Dopo la conferma il server controlla il file,
// risponde 202, si spegne in ordine e riparte nello stesso processo. La
// pagina aspetta il server nuovo (started_at diverso, o 401: le sessioni
// della dashboard stanno in memoria e si perdono) e apre il login.
// pollMs e giveUpMs ci sono per i test (tempi brevi, timer veri).
export function RestartButton({ size = 'sm', pollMs = POLL_MS, giveUpMs = GIVE_UP_MS }: { size?: 'xs' | 'sm'; pollMs?: number; giveUpMs?: number }) {
  const navigate = useNavigate()
  const [confirm, setConfirm] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // waiting: l'ora d'avvio del server vecchio, finché non risponde quello nuovo.
  const [waiting, setWaiting] = useState<{ startedAt: string } | null>(null)
  const [gaveUp, setGaveUp] = useState(false)

  async function restart() {
    setBusy(true)
    setError(null)
    try {
      const before = await api.status()
      await api.restart()
      setConfirm(false)
      setWaiting({ startedAt: before.started_at })
    } catch (err) {
      if (isUnauthorized(err)) {
        navigate('/login', { replace: true })
        return
      }
      setError(err instanceof ApiError ? err.message : 'The server does not answer.')
    } finally {
      setBusy(false)
    }
  }

  // Attesa del server nuovo. Il vecchio risponde ancora per un attimo dopo il
  // 202: si riconosce il nuovo dall'ora d'avvio, o dal 401 (sessione persa).
  useEffect(() => {
    if (waiting === null) {
      return
    }
    let timer: ReturnType<typeof setTimeout> | undefined
    let stopped = false
    const deadline = Date.now() + giveUpMs
    const poll = () => {
      api.status().then(
        (st) => {
          if (stopped) {
            return
          }
          if (st.started_at !== waiting.startedAt) {
            navigate('/devices', { replace: true }) // senza token: niente login
            return
          }
          next()
        },
        (err: unknown) => {
          if (stopped) {
            return
          }
          if (isUnauthorized(err)) {
            navigate('/login', { replace: true })
            return
          }
          next() // spento o non ancora in ascolto: si riprova
        },
      )
    }
    const next = () => {
      if (Date.now() > deadline) {
        setGaveUp(true)
        return
      }
      timer = setTimeout(poll, pollMs)
    }
    next()
    return () => {
      stopped = true
      clearTimeout(timer)
    }
  }, [waiting, navigate, pollMs, giveUpMs])

  if (waiting !== null) {
    return gaveUp ? (
      <Alert color="red" variant="light" title="The server did not come back">
        No answer after {giveUpMs / 1000} s. Check the server's log on the machine.
      </Alert>
    ) : (
      <Alert color="blue" variant="light" icon={<Loader size="xs" />}>
        Restarting the server… the page opens the login when it is back.
      </Alert>
    )
  }
  return (
    <>
      <Button size={size} color="orange" onClick={() => setConfirm(true)}>
        Restart server
      </Button>
      <Modal opened={confirm} onClose={() => setConfirm(false)} title="Restart the server" centered>
        <Stack>
          <Text size="sm">The server checks config.json, stops everything in order and starts again with it. Meanwhile:</Text>
          <List size="sm">
            <List.Item>devices fall back to LOCAL for a few seconds, then register again;</List.Item>
            <List.Item>pins are lost: devices start unpinned;</List.Item>
            <List.Item>a new recording file starts;</List.Item>
            <List.Item>you log in again (dashboard sessions are in memory).</List.Item>
          </List>
          {error !== null && (
            <Alert color="red" variant="light" title="Not restarted">
              {error}
            </Alert>
          )}
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setConfirm(false)} disabled={busy}>
              Cancel
            </Button>
            <Button color="orange" onClick={restart} loading={busy}>
              Restart
            </Button>
          </Group>
        </Stack>
      </Modal>
    </>
  )
}
