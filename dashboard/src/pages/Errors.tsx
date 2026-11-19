import { Center, Loader } from '@mantine/core'

export function NotFound() {
  return <p>Page not found.</p>
}

// RouteError: il loader è fallito per un motivo diverso dal 401 (server
// spento, errore 5xx).
export function RouteError() {
  return (
    <p style={{ padding: 16 }}>
      The SCIoT server does not answer. Check that it runs with <code>-admin-addr</code>, then reload.
    </p>
  )
}

// Loading: mentre il loader della route (requireLogin) aspetta il server,
// al primo caricamento della pagina.
export function Loading() {
  return (
    <Center h="100vh">
      <Loader size="sm" />
    </Center>
  )
}
