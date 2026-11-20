import { Alert, Badge, Button, Code, Group, Paper, ScrollArea, Stack, Text } from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router'
import { api, ApiError, isUnauthorized } from '../api/client'
import type { BuildState, BuildView } from '../api/types'
import { fmtDateTime } from '../format'

const stateColor: Record<BuildState, string> = { running: 'blue', succeeded: 'green', failed: 'red', cancelled: 'gray' }

// BuildJob: un build, dal vivo. Chiede stato e righe nuove del log ogni
// pollMs finché il build è in corso (GET …/builds/{id}?since=N: solo le
// righe dopo l'ultima già vista). Al successo propone di aggiungere il
// modello a config.json, passando dalla pagina Config (diff, controlli,
// backup), mai di nascosto. Va montato con key={id}: un altro build riparte
// da uno stato pulito.
export function BuildJob({ id, pollMs = 1000 }: { id: string; pollMs?: number }) {
  const navigate = useNavigate()
  const [view, setView] = useState<BuildView | null>(null)
  const [lines, setLines] = useState<string[]>([])
  const [error, setError] = useState<string | null>(null)
  const next = useRef(0)
  const bottom = useRef<HTMLDivElement>(null)

  useEffect(() => {
    let timer: ReturnType<typeof setTimeout> | undefined
    let stopped = false
    const poll = async () => {
      try {
        const v = await api.build(id, next.current)
        if (stopped) {
          return
        }
        next.current = v.next ?? next.current
        setView(v)
        if (v.log !== undefined && v.log.length > 0) {
          const add = v.log
          setLines((l) => [...l, ...add])
        }
        if (v.state === 'running') {
          timer = setTimeout(poll, pollMs)
        }
      } catch (err) {
        if (stopped) {
          return
        }
        if (isUnauthorized(err)) {
          navigate('/login', { replace: true })
          return
        }
        setError(err instanceof ApiError ? err.message : 'The server does not answer.')
        timer = setTimeout(poll, pollMs * 3) // il server riparte? si riprova piano
      }
    }
    poll()
    return () => {
      stopped = true
      clearTimeout(timer)
    }
  }, [id, pollMs, navigate])

  // Il log scorre da solo verso il fondo mentre arriva.
  useEffect(() => {
    bottom.current?.scrollIntoView?.({ block: 'nearest' })
  }, [lines])

  async function cancel() {
    try {
      await api.cancelBuild(id)
    } catch (err) {
      notifications.show({ color: 'red', title: 'Cancel failed', message: err instanceof ApiError ? err.message : 'The server does not answer.' })
    }
  }

  if (view === null) {
    return error === null ? <Text c="dimmed">Loading…</Text> : <Alert color="red">{error}</Alert>
  }
  return (
    <Stack>
      <Group justify="space-between">
        <Group>
          <Text fw={500}>{view.model_key}</Text>
          <Badge color={stateColor[view.state]} variant="light">
            {view.state}
          </Badge>
        </Group>
        <Text size="sm" c="dimmed">
          by {view.who} · {fmtDateTime(view.started_at)}
          {view.exit_code !== undefined && ` · exit ${view.exit_code}`}
        </Text>
      </Group>
      {view.state === 'running' && (
        <Group>
          <Button variant="default" color="red" onClick={cancel}>
            Cancel the build
          </Button>
        </Group>
      )}
      {view.state === 'succeeded' && (
        <Alert color="green" variant="light" title="Model built">
          <Stack gap="xs" align="flex-start">
            <Text size="sm">
              {view.output} · model_hash <Code>{view.model_hash}</Code>. The server does not use it yet: add it to config.json, then restart.
            </Text>
            <Button size="xs" onClick={() => navigate('/config', { state: { addModel: { model_key: view.model_key, tflite_dir: view.output } } })}>
              Add to config.json
            </Button>
          </Stack>
        </Alert>
      )}
      {view.state === 'failed' && (
        <Alert color="red" variant="light" title="Build failed">
          The reason is at the end of the log (docs/MODELS.md, Troubleshooting).
        </Alert>
      )}
      <Paper withBorder>
        <ScrollArea h={360} type="auto">
          <Code block style={{ whiteSpace: 'pre' }}>
            {lines.length === 0 ? '(no output yet)' : lines.join('\n')}
          </Code>
          <div ref={bottom} />
        </ScrollArea>
      </Paper>
    </Stack>
  )
}
