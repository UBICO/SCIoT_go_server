import { Alert, Button, Group, JsonInput, Modal, Stack, Text, Title } from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { useState } from 'react'
import { useLoaderData, useLocation, useNavigate, useRevalidator } from 'react-router'
import { api, ApiError, isUnauthorized } from '../api/client'
import type { ConfigFile, ConfigSaveResponse } from '../api/types'
import { ConfigDiff } from '../components/ConfigDiff'
import { RestartButton } from '../components/RestartButton'
import { type AddModel, withModel } from '../configEdit'

// ConfigPage: config.json del server, da modificare (docs/DASHBOARD.md).
//
// Il server controlla il testo con gli stessi controlli dell'avvio
// (Validate, e di nuovo al salvataggio), rifiuta un cambio di
// worker.command, fa un backup del file e lo scrive in modo atomico. Il
// server in esecuzione non cambia: la config salvata vale dal prossimo
// avvio.

// I dati arrivano da configLoader (router.tsx); null = modifica spenta.
export function ConfigPage() {
  const file = useLoaderData() as ConfigFile | null
  const revalidator = useRevalidator()
  const navigate = useNavigate()
  const addModel = (useLocation().state as { addModel?: AddModel } | null)?.addModel
  // saved sta qui e non nell'editor: l'editor si ricrea a ogni versione nuova
  // del file (key), l'avviso deve restare.
  const [saved, setSaved] = useState<ConfigSaveResponse | null>(null)

  if (file === null) {
    return <Text>Config editing is not enabled on this server.</Text>
  }
  return (
    <Stack>
      <Title order={3}>Config</Title>
      <Text size="sm" c="dimmed">
        {file.path}
      </Text>
      {saved !== null && saved.changed && (
        <Alert color="green" variant="light" title="Saved">
          <Stack gap="xs" align="flex-start">
            <Text size="sm">
              The server still runs with the configuration it started with: restart it to apply the new one.
              {saved.backup !== undefined && ` The previous file is in ${saved.backup}, next to it.`}
            </Text>
            <RestartButton size="xs" />
          </Stack>
        </Alert>
      )}
      {saved === null && file.modified_since_start && (
        <Alert color="yellow" variant="light">
          <Stack gap="xs" align="flex-start">
            <Text size="sm">The file differs from the configuration the server started with: restart the server to apply it.</Text>
            <RestartButton size="xs" />
          </Stack>
        </Alert>
      )}
      {addModel !== undefined && <AddModelNotice file={file} model={addModel} />}
      {/* key: una versione nuova del file (salvata, o ricaricata) riparte da zero. */}
      <ConfigEditor
        key={file.hash}
        file={file}
        initial={addModel === undefined ? undefined : textOrUndefined(withModel(file.content, addModel))}
        onSaved={(res) => {
          setSaved(res)
          // Via lo stato della navigazione: il modello è salvato, ricaricando
          // non va aggiunto di nuovo. navigate ricarica anche il file.
          if (addModel !== undefined) {
            navigate('/config', { replace: true, state: null })
            return
          }
          revalidator.revalidate()
        }}
        onReload={() => {
          setSaved(null)
          revalidator.revalidate()
        }}
      />
    </Stack>
  )
}

function textOrUndefined(r: { text: string } | { reason: string }): string | undefined {
  return 'text' in r ? r.text : undefined
}

function AddModelNotice({ file, model }: { file: ConfigFile; model: AddModel }) {
  const r = withModel(file.content, model)
  if ('text' in r) {
    return (
      <Alert color="blue" variant="light" title={`Add ${model.model_key}`}>
        The model is added to "models" below (the file is reformatted with 2-space indentation). Review the changes, Validate, Save, then restart the server.
      </Alert>
    )
  }
  return (
    <Alert color="yellow" variant="light" title={`Add ${model.model_key} by hand`}>
      {r.reason}. Add to "models": {JSON.stringify(model)}
    </Alert>
  )
}

type Check = { state: 'valid' } | { state: 'invalid'; message: string } | null

function ConfigEditor({ file, initial, onSaved, onReload }: { file: ConfigFile; initial?: string; onSaved: (r: ConfigSaveResponse) => void; onReload: () => void }) {
  const navigate = useNavigate()
  const [text, setText] = useState(initial ?? file.content)
  const [check, setCheck] = useState<Check>(null) // l'esito dell'ultimo Validate, per il testo di allora
  const [conflict, setConflict] = useState(false) // 409: il file è cambiato sul server
  const [confirm, setConfirm] = useState(false)
  const [busy, setBusy] = useState(false)
  const changed = text !== file.content

  // fail gestisce gli errori comuni: sessione scaduta, server muto.
  function fail(err: unknown): boolean {
    if (isUnauthorized(err)) {
      navigate('/login', { replace: true })
      return true
    }
    if (!(err instanceof ApiError)) {
      notifications.show({ color: 'red', title: 'Error', message: 'The server does not answer.' })
      return true
    }
    return false
  }

  async function validate() {
    setBusy(true)
    try {
      await api.validateConfig(text)
      setCheck({ state: 'valid' })
    } catch (err) {
      if (!fail(err) && err instanceof ApiError) {
        setCheck({ state: 'invalid', message: err.message })
      }
    } finally {
      setBusy(false)
    }
  }

  async function save() {
    setBusy(true)
    try {
      const res = await api.saveConfig(text, file.hash)
      notifications.show({ color: 'green', title: 'Config saved', message: res.backup === undefined ? 'No change.' : `Backup: ${res.backup}` })
      onSaved(res)
    } catch (err) {
      if (!fail(err) && err instanceof ApiError) {
        if (err.code === 'config_changed') {
          setConflict(true)
        } else {
          setCheck({ state: 'invalid', message: err.message })
        }
      }
    } finally {
      setBusy(false)
      setConfirm(false)
    }
  }

  return (
    <Stack>
      {conflict && (
        <Alert color="red" variant="light" title="The file changed on the server">
          <Stack gap="xs">
            <Text size="sm">Someone changed config.json after you loaded it. Copy your edits, reload the file, and apply them again.</Text>
            <Group>
              <Button size="xs" variant="default" onClick={onReload}>
                Reload the file (your edits are lost)
              </Button>
            </Group>
          </Stack>
        </Alert>
      )}
      <JsonInput
        aria-label="config.json"
        value={text}
        onChange={(v) => {
          setText(v)
          setCheck(null) // l'esito valeva per il testo di prima
        }}
        validationError="Not valid JSON"
        autosize
        minRows={12}
        maxRows={40}
        spellCheck={false}
      />
      {check?.state === 'valid' && (
        <Alert color="green" variant="light">
          Valid: the server would start with this configuration.
        </Alert>
      )}
      {check?.state === 'invalid' && (
        <Alert color="red" variant="light" title="Not valid">
          {check.message}
        </Alert>
      )}
      <Group>
        <Button variant="default" onClick={validate} loading={busy && !confirm}>
          Validate
        </Button>
        <Button onClick={() => setConfirm(true)} disabled={!changed || conflict}>
          Save
        </Button>
        <Button variant="subtle" onClick={() => setText(file.content)} disabled={!changed}>
          Discard changes
        </Button>
      </Group>
      {changed && (
        <Stack gap={4}>
          <Text size="sm" fw={500}>
            Changes
          </Text>
          <ConfigDiff before={file.content} after={text} />
        </Stack>
      )}

      <Modal opened={confirm} onClose={() => setConfirm(false)} title="Save config.json" size="lg" centered>
        <Stack>
          <Text size="sm">The server checks the file again, keeps a backup of the current one, and writes it. It applies at the next start.</Text>
          <ConfigDiff before={file.content} after={text} />
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setConfirm(false)} disabled={busy}>
              Cancel
            </Button>
            <Button onClick={save} loading={busy}>
              Save
            </Button>
          </Group>
        </Stack>
      </Modal>
    </Stack>
  )
}
