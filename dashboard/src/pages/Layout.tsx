import { AppShell, Badge, Burger, Button, Group, NavLink, Text, Title } from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { NavLink as RouterLink, Outlet, useLoaderData, useNavigate } from 'react-router'
import { api } from '../api/client'
import type { ServerStatus } from '../api/types'
import { fmtDuration, fmtPercent } from '../format'
import { type Connection, useLive } from '../live/context'
import { LiveProvider } from '../live/LiveProvider'

// Layout: la cornice delle pagine protette (header, menu a sinistra) e lo
// stream degli eventi, aperto una volta per tutte le pagine. Il loader della
// route (requireLogin in router.tsx) ha già verificato il login.
export function Layout() {
  return (
    <LiveProvider>
      <Shell />
    </LiveProvider>
  )
}

// Le voci del menu. Una pagina nuova si aggiunge qui e in router.tsx.
const nav = [
  { to: '/devices', label: 'Devices' },
  { to: '/models', label: 'Models' },
  { to: '/recordings', label: 'Recordings' },
  { to: '/changelog', label: 'Change log' },
  { to: '/config', label: 'Config' },
]

function Shell() {
  const [opened, { toggle, close }] = useDisclosure()
  const navigate = useNavigate()
  const first = useLoaderData() as ServerStatus
  const { state, connection } = useLive()
  const status = state.status ?? first

  async function logout() {
    try {
      await api.logout()
    } finally {
      navigate('/login', { replace: true })
    }
  }

  return (
    <AppShell header={{ height: 56 }} navbar={{ width: 200, breakpoint: 'sm', collapsed: { mobile: !opened } }} padding="md">
      <AppShell.Header>
        <Group h="100%" px="md" justify="space-between" wrap="nowrap">
          <Group wrap="nowrap">
            <Burger opened={opened} onClick={toggle} hiddenFrom="sm" size="sm" aria-label="Menu" />
            <Title order={4}>SCIoT</Title>
            <ConnectionBadge connection={connection} />
          </Group>
          <Group wrap="nowrap" gap="lg">
            <Text size="sm" c="dimmed" visibleFrom="sm">
              {status.devices} devices · server CPU {fmtPercent(status.load.cpu_percent)} · up {fmtDuration(status.uptime_seconds)}
            </Text>
            <Button variant="subtle" size="xs" onClick={logout}>
              Log out
            </Button>
          </Group>
        </Group>
      </AppShell.Header>
      <AppShell.Navbar p="xs">
        {nav.map((n) => (
          <NavLink key={n.to} component={RouterLink} to={n.to} label={n.label} onClick={close} />
        ))}
      </AppShell.Navbar>
      <AppShell.Main>
        <Outlet />
      </AppShell.Main>
    </AppShell>
  )
}

const connectionBadge: Record<Connection, { color: string; label: string }> = {
  open: { color: 'green', label: 'live' },
  connecting: { color: 'yellow', label: 'connecting' },
  closed: { color: 'red', label: 'disconnected' },
}

function ConnectionBadge({ connection }: { connection: Connection }) {
  const b = connectionBadge[connection]
  return (
    <Badge color={b.color} variant="light">
      {b.label}
    </Badge>
  )
}
