import { Badge, Table, Text, Title } from '@mantine/core'
import { useLoaderData } from 'react-router'
import type { ChangeLogEntry, ChangeLogResponse } from '../api/types'
import { fmtDateTime, fmtPolicy } from '../format'

// ChangeLogPage: chi ha cambiato cosa dalla porta admin (pin, unpin, config,
// riavvio), dalla più recente (ADMIN_API.md#change-log). I dati arrivano da
// changelogLoader (router.tsx): le ultime 200 righe.
export function ChangeLogPage() {
  const data = useLoaderData() as ChangeLogResponse
  return (
    <>
      <Title order={3} mb="md">
        Change log
      </Title>
      {!data.enabled ? (
        <Text>The change log is written next to the recordings: add a "record" section to config.json. Meanwhile actions are only in the server's log.</Text>
      ) : data.entries.length === 0 ? (
        <Text c="dimmed">No change yet.</Text>
      ) : (
        <Table.ScrollContainer minWidth={640}>
          <Table striped>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Time</Table.Th>
                <Table.Th>Who</Table.Th>
                <Table.Th>Action</Table.Th>
                <Table.Th>What</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {data.entries.map((e, i) => (
                <Table.Tr key={`${e.ts}-${i}`}>
                  <Table.Td>{fmtDateTime(e.ts)}</Table.Td>
                  <Table.Td>{e.who}</Table.Td>
                  <Table.Td>
                    <Badge variant="light" color={actionColor[e.action] ?? 'gray'}>
                      {e.action}
                    </Badge>
                  </Table.Td>
                  <Table.Td>{what(e)}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
    </>
  )
}

const actionColor: Record<string, string> = { pin: 'orange', unpin: 'blue', config: 'grape', restart: 'red' }

// what: l'oggetto dell'azione, in una riga.
function what(e: ChangeLogEntry): string {
  if (e.config !== undefined) {
    const backup = e.config.backup === undefined ? '' : ` (backup ${e.config.backup})`
    return `${e.config.path}${backup}`
  }
  if (e.device_id !== undefined) {
    return e.policy === undefined ? e.device_id : `${e.device_id}: ${fmtPolicy(e.policy)}`
  }
  return '–'
}
