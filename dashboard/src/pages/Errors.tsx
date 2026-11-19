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
