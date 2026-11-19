import { Alert, Button, Center, Paper, PasswordInput, Stack, TextInput, Title } from '@mantine/core'
import { type FormEvent, useState } from 'react'
import { useNavigate } from 'react-router'
import { api, ApiError } from '../api/client'

// LoginPage: token (SCIOT_ADMIN_TOKEN) e nome. Il nome serve a sapere chi
// ha cambiato cosa (un solo token condiviso, docs/ADMIN_API.md). Il token
// non si salva nel browser: il server risponde con un cookie di sessione
// HttpOnly, che la pagina non vede.
export function LoginPage() {
  const navigate = useNavigate()
  const [token, setToken] = useState('')
  const [name, setName] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function submit(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await api.login(token, name.trim())
      navigate('/devices', { replace: true })
    } catch (err) {
      setError(loginError(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Center h="100vh" p="md">
      <Paper withBorder p="xl" w={380}>
        <form onSubmit={submit}>
          <Stack>
            <Title order={3}>SCIoT dashboard</Title>
            <TextInput label="Name" description="Who you are, for the change log" value={name} onChange={(e) => setName(e.currentTarget.value)} required autoComplete="username" />
            <PasswordInput label="Admin token" description="SCIOT_ADMIN_TOKEN of the server" value={token} onChange={(e) => setToken(e.currentTarget.value)} autoComplete="current-password" />
            {error !== null && (
              <Alert color="red" variant="light">
                {error}
              </Alert>
            )}
            <Button type="submit" loading={busy} disabled={name.trim() === ''}>
              Log in
            </Button>
          </Stack>
        </form>
      </Paper>
    </Center>
  )
}

function loginError(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 401) {
      return 'Wrong token.'
    }
    return `${err.message} (${err.code})`
  }
  return 'The server does not answer.'
}
