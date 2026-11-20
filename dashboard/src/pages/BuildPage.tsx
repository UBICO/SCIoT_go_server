import { Alert, Anchor, Badge, Button, FileInput, Group, NumberInput, Paper, Stack, TagsInput, Text, TextInput, Title } from '@mantine/core'
import { type FormEvent, useState } from 'react'
import { Link, useLoaderData, useNavigate, useParams } from 'react-router'
import { api, ApiError, isUnauthorized } from '../api/client'
import type { BuildsResponse } from '../api/types'
import { BuildJob } from '../components/BuildJob'
import { fmtDateTime } from '../format'

// La chiave del modello: anche il nome della cartella (come nel server).
const KEY = /^[a-z0-9][a-z0-9_-]{0,63}$/

// BuildPage: costruisce un modello dai due export di Edge Impulse
// (docs/MODELS.md) con sciot-model build sul server. Con :id mostra un build.
// I dati (abilitato, models_dir, build recenti) da buildsLoader (router.tsx).
export function BuildPage() {
  const data = useLoaderData() as BuildsResponse
  const { id } = useParams()

  if (!data.enabled) {
    return (
      <>
        <Title order={3} mb="md">
          Build a model
        </Title>
        <Text>Model builds are off: set "models_dir" in config.json to the folder where models are built (DASHBOARD.md).</Text>
      </>
    )
  }
  return (
    <Stack>
      <Title order={3}>Build a model</Title>
      {id === undefined ? <BuildForm modelsDir={data.models_dir ?? ''} running={data.builds.some((b) => b.state === 'running')} /> : <BuildJob key={id} id={id} />}
      {data.builds.length > 0 && (
        <Paper withBorder p="sm">
          <Text fw={500} mb="xs">
            Recent builds (since the server started)
          </Text>
          <Stack gap={4}>
            {data.builds.map((b) => (
              <Group key={b.id} gap="xs">
                <Anchor component={Link} to={`/models/builds/${encodeURIComponent(b.id)}`}>
                  {b.model_key}
                </Anchor>
                <Badge size="sm" variant="light">
                  {b.state}
                </Badge>
                <Text size="sm" c="dimmed">
                  {b.who} · {fmtDateTime(b.started_at)}
                </Text>
              </Group>
            ))}
          </Stack>
        </Paper>
      )}
    </Stack>
  )
}

function BuildForm({ modelsDir, running }: { modelsDir: string; running: boolean }) {
  const navigate = useNavigate()
  const [key, setKey] = useState('')
  const [savedModel, setSavedModel] = useState<File | null>(null)
  const [cppExport, setCppExport] = useState<File | null>(null)
  const [samples, setSamples] = useState<File[]>([])
  const [maxSegments, setMaxSegments] = useState<number | string>(0)
  const [cuts, setCuts] = useState<string[]>([])
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const keyError = key !== '' && !KEY.test(key) ? 'lowercase letters, digits, "_" and "-"; it is also the folder name' : null
  const ready = KEY.test(key) && savedModel !== null && cppExport !== null && !running

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (!ready) {
      return
    }
    const form = new FormData()
    form.append('model_key', key)
    form.append('savedmodel', savedModel!)
    form.append('cpp_export', cppExport!)
    for (const s of samples) {
      form.append('sample', s)
    }
    if (Number(maxSegments) > 0) {
      form.append('max_segments', String(maxSegments))
    }
    for (const c of cuts) {
      form.append('cut', c)
    }
    setBusy(true)
    setError(null)
    try {
      const job = await api.startBuild(form)
      navigate(`/models/builds/${encodeURIComponent(job.id)}`)
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

  return (
    <form onSubmit={submit}>
      <Stack>
        <Alert color="yellow" variant="light">
          The build runs on the server and loads the CPU for minutes: do not build during a measurement run. One build at a time.
        </Alert>
        {running && <Alert color="blue">A build is running: wait for it, or cancel it from its page.</Alert>}
        <TextInput label="Model key" description={`The model folder will be ${modelsDir}/<key>`} placeholder="yolo_small_320" value={key} onChange={(e) => setKey(e.currentTarget.value)} error={keyError} required />
        <FileInput label="SavedModel export" description='Edge Impulse "TensorFlow SavedModel" (.zip)' accept=".zip" value={savedModel} onChange={setSavedModel} required clearable />
        <FileInput label="C++ library export" description='Edge Impulse "C++ library (Linux)", TensorFlow Lite, unoptimized float32 (.zip), same model version' accept=".zip" value={cppExport} onChange={setCppExport} required clearable />
        <FileInput label="Sample images" description="Real images for the verification (up to 10; strongly recommended)" accept="image/*" multiple value={samples} onChange={setSamples} clearable />
        <Group grow align="flex-start">
          <NumberInput label="Max segments" description="0 = all automatic cuts" min={0} max={64} value={maxSegments} onChange={setMaxSegments} />
          <TagsInput label="Cuts" description="Tensor names to cut at (advanced)" value={cuts} onChange={setCuts} />
        </Group>
        {error !== null && (
          <Alert color="red" variant="light" title="Not started">
            {error}
          </Alert>
        )}
        <Group>
          <Button type="submit" loading={busy} disabled={!ready}>
            Upload and build
          </Button>
        </Group>
      </Stack>
    </form>
  )
}
