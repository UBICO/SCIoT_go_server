import { Alert, Anchor, Badge, Group, Loader, Paper, SimpleGrid, Stack, Table, Text, Title } from '@mantine/core'
import { Link, useParams } from 'react-router'
import type { AdminDecision, AdminDevice, Prediction } from '../api/types'
import { INK, type SeriesSpec } from '../components/chartData'
import { PolicyBadges } from '../components/PolicyBadges'
import { TimeChart } from '../components/TimeChart'
import { fmtAge, fmtBandwidth, fmtBytes, fmtMs, fmtNumber, fmtPercent, fmtPolicy, fmtTime } from '../format'
import { useNow } from '../hooks'
import { useLive } from '../live/context'
import { HISTORY_SECONDS, LOCAL_LEVEL } from '../live/state'

// I colori delle linee: rosso, blu, verde, grigio, nero (la palette dei
// grafici del progetto). Il nero è INK: diventa chiaro sul tema scuro.
const RED = '#e03131'
const BLUE = '#1c7ed6'
const GREEN = '#2f9e44'
const GREY = '#868e96'
const BLACK = INK

// Le serie dei grafici: costanti di modulo (vedi SeriesSpec).
const latencySeries: SeriesSpec[] = [
  { label: 'inference (mean)', color: BLUE, value: (s) => s.inferenceMs },
  { label: 'inference (max)', color: RED, value: (s) => s.inferenceMaxMs },
  { label: 'answer', color: GREEN, value: (s) => s.answerMs },
  { label: 'RTT', color: GREY, value: (s) => s.rttMs },
  { label: 'network latency', color: BLACK, value: (s) => s.networkLatencyMs },
]
const cpuSeries: SeriesSpec[] = [{ label: 'device CPU', color: BLUE, value: (s) => s.cpuPercent }]
const bandwidthSeries: SeriesSpec[] = [{ label: 'bandwidth', color: BLUE, value: (s) => s.bandwidthMBps }]
const policySeries: SeriesSpec[] = [{ label: 'split point', color: BLUE, value: (s) => s.split, stepped: true }]
const policyAxis = (v: number) => (v === LOCAL_LEVEL ? 'LOCAL' : Number.isInteger(v) && v >= -1 ? `@${v}` : '')
// Scala della policy: sempre LOCAL e almeno lo split 0, con mezzo passo di
// margine, così una linea piatta non finisce sul bordo.
const policyRange = (_u: unknown, min: number, max: number): [number, number] => [Math.min(min, LOCAL_LEVEL) - 0.5, Math.max(max, 0) + 0.5]

// DeviceDetailPage: un device, dal vivo. I grafici partono da quando la
// pagina è aperta (la storia è solo nel browser, live/state.ts).
export function DeviceDetailPage() {
  const { id = '' } = useParams()
  const { state } = useLive()
  const live = state.devices[id]

  if (live === undefined) {
    if (state.status === null) {
      return <Loader size="sm" />
    }
    return (
      <Text>
        Device <b>{id}</b> is not registered. <Anchor component={Link} to="/devices">All devices</Anchor>
      </Text>
    )
  }
  const d = live.device
  return (
    <Stack>
      <Group justify="space-between">
        <Group>
          <Title order={3}>{d.device_id}</Title>
          <PolicyBadges device={d} />
        </Group>
        <Text size="sm" c="dimmed">
          {d.device_type} · {d.model_key} · session {d.session_id.slice(0, 8)}
        </Text>
      </Group>

      <Stats device={d} />

      <Text size="xs" c="dimmed">
        Charts: one point per heartbeat, the last {HISTORY_SECONDS / 60} minutes since the dashboard was opened (the history lives in this browser tab).
      </Text>
      <SimpleGrid cols={{ base: 1, lg: 2 }}>
        <TimeChart title="Latency (ms)" unit="ms" samples={live.samples} series={latencySeries} />
        <TimeChart title="Policy (server)" unit="" samples={live.samples} series={policySeries} yValues={policyAxis} yRange={policyRange} />
        <TimeChart title="Device CPU (%)" unit="%" samples={live.samples} series={cpuSeries} />
        <TimeChart title="Bandwidth measured by the server (MB/s)" unit="MB/s" samples={live.samples} series={bandwidthSeries} />
      </SimpleGrid>

      {d.pinned && (
        <Alert color="orange" variant="light">
          Pinned by the admin since {fmtTime(d.policy_since)}: the algorithm does not decide for this device. The decision below is the last one before the pin.
        </Alert>
      )}
      <LastDecision decision={d.last_decision} />
      <PolicyChanges changes={live.changes} />
    </Stack>
  )
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <Paper withBorder p="xs">
      <Text size="xs" c="dimmed">
        {label}
      </Text>
      <Text fw={500}>{value}</Text>
    </Paper>
  )
}

// Stats: l'ultimo heartbeat (telemetry, come lo manda il device) e la rete
// misurata dal server.
function Stats({ device: d }: { device: AdminDevice }) {
  const now = useNow()
  const t = d.telemetry
  const n = d.network
  return (
    <SimpleGrid cols={{ base: 2, sm: 4, lg: 8 }}>
      <Stat label="Last seen" value={fmtAge((now - Date.parse(d.last_seen)) / 1000)} />
      <Stat label="Inferences" value={t === null ? '–' : String(t.inference_count)} />
      <Stat label="Inference mean / max" value={`${fmtMs(t?.inference_time_ms)} / ${fmtMs(t?.inference_time_max_ms)}`} />
      <Stat label="Answer" value={fmtMs(t?.answer_ms)} />
      <Stat label="RTT" value={fmtMs(t?.rtt_ms)} />
      <Stat label="CPU" value={fmtPercent(t?.cpu_load === undefined ? null : t.cpu_load * 100)} />
      <Stat label="Device memory free" value={fmtBytes(t?.memory_available_bytes)} />
      <Stat label="Bandwidth" value={n?.not_limiting ? `${fmtBandwidth(n.bandwidth_bps)} (not limiting)` : fmtBandwidth(n?.bandwidth_bps)} />
    </SimpleGrid>
  )
}

// LastDecision: il motivo dell'ultima decisione e le previsioni del modello
// di costo per ogni split (COSTMODEL.md). La riga scelta è evidenziata.
function LastDecision({ decision }: { decision: AdminDecision | null }) {
  if (decision === null) {
    return (
      <Paper withBorder p="sm">
        <Text size="sm" c="dimmed">
          No decision yet (the static algorithm never decides).
        </Text>
      </Paper>
    )
  }
  const dec = decision.decision
  const chosen = (p: Prediction) => (dec.mode === 'LOCAL' ? p.local : !p.local && p.split_point === dec.split_point && (dec.max_inflight === undefined || p.window === dec.max_inflight))
  return (
    <Paper withBorder p="sm">
      <Group justify="space-between" mb="xs">
        <Text fw={500}>
          Last decision: {fmtPolicy(dec.current)} → {fmtPolicy(dec)}
          {dec.changed ? '' : ' (unchanged)'}
        </Text>
        <Text size="sm" c="dimmed">
          {dec.algorithm} · {fmtTime(decision.at)}
          {dec.lag_ms === undefined ? '' : ` · lag ${fmtMs(dec.lag_ms)}`}
        </Text>
      </Group>
      <Text size="sm" mb="xs">
        {dec.reason}
      </Text>
      {dec.predictions !== undefined && dec.predictions.length > 0 && (
        <Table.ScrollContainer minWidth={720}>
          <Table striped>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Split</Table.Th>
                <Table.Th>Total</Table.Th>
                <Table.Th>p95 (predicted)</Table.Th>
                <Table.Th>Device</Table.Th>
                <Table.Th>Upload</Table.Th>
                <Table.Th>Wait</Table.Th>
                <Table.Th>Server</Table.Th>
                <Table.Th>Frames/s</Table.Th>
                <Table.Th>Bound</Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {dec.predictions.map((p) => (
                <Table.Tr key={`${p.local ? 'L' : p.split_point}-${p.window ?? 0}`} fw={chosen(p) ? 700 : undefined}>
                  <Table.Td>
                    {p.local ? 'LOCAL' : `@${p.split_point}${p.window === undefined ? '' : ` ×${p.window}`}`}
                    {p.saturated && (
                      <Badge ml={4} size="xs" color="red" variant="light">
                        saturated
                      </Badge>
                    )}
                  </Table.Td>
                  <Table.Td>{fmtMs(p.total_ms)}</Table.Td>
                  <Table.Td>{fmtMs(p.p95_ms)}</Table.Td>
                  <Table.Td>{fmtMs(p.device_ms)}</Table.Td>
                  <Table.Td>{fmtMs(p.upload_ms)}</Table.Td>
                  <Table.Td>{fmtMs(p.wait_ms)}</Table.Td>
                  <Table.Td>{fmtMs(p.server_ms)}</Table.Td>
                  <Table.Td>{fmtNumber(p.frames_per_s)}</Table.Td>
                  <Table.Td>{p.bound ?? '–'}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
    </Paper>
  )
}

// PolicyChanges: le decisioni che hanno cambiato la policy, viste da quando
// la dashboard è aperta.
function PolicyChanges({ changes }: { changes: AdminDecision[] }) {
  if (changes.length === 0) {
    return null
  }
  return (
    <Paper withBorder p="sm">
      <Text fw={500} mb="xs">
        Policy changes
      </Text>
      <Table.ScrollContainer minWidth={600}>
        <Table>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Time</Table.Th>
              <Table.Th>Policy</Table.Th>
              <Table.Th>Reason</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {changes.map((a) => (
              <Table.Tr key={a.at}>
                <Table.Td>{fmtTime(a.at)}</Table.Td>
                <Table.Td>
                  {fmtPolicy(a.decision.current)} → {fmtPolicy(a.decision)}
                </Table.Td>
                <Table.Td>{a.decision.reason}</Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Table.ScrollContainer>
    </Paper>
  )
}
