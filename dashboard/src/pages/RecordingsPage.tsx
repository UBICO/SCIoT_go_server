import { Anchor, Badge, Table, Text, Title } from '@mantine/core'
import { useLoaderData } from 'react-router'
import { recordingURL } from '../api/client'
import type { RecordingsResponse } from '../api/types'
import { fmtDateTime, fmtFileSize } from '../format'

// RecordingsPage: le registrazioni di record.dir, una per avvio del server,
// da scaricare (RECORDING.md). Sola lettura: si cancellano a mano sulla
// macchina. I dati arrivano da recordingsLoader (router.tsx).
export function RecordingsPage() {
  const data = useLoaderData() as RecordingsResponse
  if (!data.recording) {
    return (
      <>
        <Title order={3} mb="md">
          Recordings
        </Title>
        <Text>Recording is off: add a "record" section to config.json (RECORDING.md).</Text>
      </>
    )
  }
  return (
    <>
      <Title order={3}>Recordings</Title>
      <Text size="sm" c="dimmed" mb="md">
        {data.dir} · one file per server start · read them with sciot-replay
      </Text>
      {data.files.length === 0 ? (
        <Text c="dimmed">No recording yet.</Text>
      ) : (
        <Table.ScrollContainer minWidth={560}>
          <Table highlightOnHover>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>File</Table.Th>
                <Table.Th>Size</Table.Th>
                <Table.Th>Last write</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {data.files.map((f) => (
                <Table.Tr key={f.name}>
                  <Table.Td>
                    <Anchor href={recordingURL(f.name)} download={f.name}>
                      {f.name}
                    </Anchor>
                    {f.current && (
                      <Badge ml="xs" size="sm" color="green" variant="light" title="Being written: the download stops at the last complete line">
                        recording
                      </Badge>
                    )}
                  </Table.Td>
                  <Table.Td>{fmtFileSize(f.size_bytes)}</Table.Td>
                  <Table.Td>{fmtDateTime(f.modified)}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
    </>
  )
}
