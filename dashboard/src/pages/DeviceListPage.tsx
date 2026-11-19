import { Anchor, Loader, Table, Text, Title } from '@mantine/core'
import { Link, useNavigate } from 'react-router'
import { PolicyBadges } from '../components/PolicyBadges'
import { fmtAge, fmtMs } from '../format'
import { useNow } from '../hooks'
import { useLive } from '../live/context'

// DeviceListPage: tutti i device registrati, ordinati per id, con policy,
// ultimo heartbeat e motivo dell'ultima decisione. Un clic apre il device.
export function DeviceListPage() {
  const { state } = useLive()
  const navigate = useNavigate()
  const now = useNow()
  const devices = Object.values(state.devices)
    .map((l) => l.device)
    .sort((a, b) => a.device_id.localeCompare(b.device_id))

  if (state.status === null) {
    return <Loader size="sm" /> // prima istantanea dello stream non ancora arrivata
  }
  return (
    <>
      <Title order={3} mb="md">
        Devices
      </Title>
      {devices.length === 0 ? (
        <Text c="dimmed">No device registered.</Text>
      ) : (
        <Table.ScrollContainer minWidth={760}>
          <Table highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Device</Table.Th>
                <Table.Th>Type</Table.Th>
                <Table.Th>Model</Table.Th>
                <Table.Th>Policy</Table.Th>
                <Table.Th>Inference</Table.Th>
                <Table.Th>Last seen</Table.Th>
                <Table.Th>Last decision</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {devices.map((d) => (
                // Tutta la riga è cliccabile; il link sull'id serve alla tastiera.
                <Table.Tr key={d.device_id} style={{ cursor: 'pointer' }} onClick={() => navigate(deviceURL(d.device_id))}>
                  <Table.Td fw={500}>
                    <Anchor component={Link} to={deviceURL(d.device_id)} onClick={(e) => e.stopPropagation()}>
                      {d.device_id}
                    </Anchor>
                  </Table.Td>
                  <Table.Td>{d.device_type}</Table.Td>
                  <Table.Td>{d.model_key}</Table.Td>
                  <Table.Td>
                    <PolicyBadges device={d} />
                  </Table.Td>
                  <Table.Td>{fmtMs(d.telemetry?.inference_time_ms)}</Table.Td>
                  <Table.Td>{fmtAge((now - Date.parse(d.last_seen)) / 1000)}</Table.Td>
                  <Table.Td>
                    <Text size="sm" lineClamp={1}>
                      {d.last_decision?.decision.reason ?? '–'}
                    </Text>
                  </Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
    </>
  )
}

function deviceURL(id: string): string {
  return `/devices/${encodeURIComponent(id)}`
}
