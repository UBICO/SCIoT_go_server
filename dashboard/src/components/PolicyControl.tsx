import { Button, Group, Modal, NativeSelect, Paper, SegmentedControl, Stack, Text } from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { useState } from 'react'
import { useNavigate } from 'react-router'
import { api, ApiError, isUnauthorized } from '../api/client'
import type { AdminDevice, Mode, SetPolicyRequest } from '../api/types'
import { fmtPolicy } from '../format'

// PolicyControl: pin e unpin di un device (OFFLOAD.md#admin).
//
// Pin = la policy la imponi tu: l'algoritmo non tocca più il device finché
// non fai unpin. Il server la valida (split e finestra) e la consegna al
// device con il prossimo heartbeat; la pagina la vede arrivare dallo
// stream. Unpin = il device torna all'algoritmo, la policy resta finché
// l'algoritmo non decide altrimenti.
//
// Il modulo parte dalla policy attuale e poi non la segue più: se
// l'algoritmo la cambia mentre stai scegliendo, la tua scelta resta.
export function PolicyControl({ device }: { device: AdminDevice }) {
  const navigate = useNavigate()
  const [mode, setMode] = useState<Mode>(device.policy.mode)
  const [split, setSplit] = useState(String(device.policy.split_point ?? device.last_offloading_layer - 1))
  const [win, setWin] = useState(device.policy.max_inflight === undefined ? '' : String(device.policy.max_inflight)) // '' = il massimo
  const [confirm, setConfirm] = useState<'pin' | 'unpin' | null>(null)
  const [busy, setBusy] = useState(false)

  const req = pinRequest(mode, split, win)

  async function run(action: 'pin' | 'unpin') {
    setBusy(true)
    try {
      const res = action === 'pin' ? await api.pin(device.device_id, req) : await api.unpin(device.device_id)
      notifications.show({
        color: 'green',
        title: action === 'pin' ? 'Pinned' : 'Unpinned',
        message: `${res.device_id}: ${fmtPolicy(res.policy)} (v${res.policy.policy_version})`,
      })
    } catch (err) {
      if (isUnauthorized(err)) {
        navigate('/login', { replace: true })
        return
      }
      notifications.show({
        color: 'red',
        title: action === 'pin' ? 'Pin refused' : 'Unpin failed',
        message: err instanceof ApiError ? err.message : 'The server does not answer.',
      })
    } finally {
      setBusy(false)
      setConfirm(null)
    }
  }

  return (
    <Paper withBorder p="sm">
      <Text fw={500} mb="xs">
        Policy
      </Text>
      <Group align="flex-end">
        <SegmentedControl
          aria-label="Mode"
          value={mode}
          onChange={(v) => setMode(v as Mode)}
          data={[
            { value: 'LOCAL', label: 'LOCAL' },
            { value: 'SPLIT', label: 'SPLIT' },
          ]}
        />
        <NativeSelect label="Split point" value={split} onChange={(e) => setSplit(e.currentTarget.value)} disabled={mode === 'LOCAL'} data={splitOptions(device.last_offloading_layer)} />
        {/* Senza data plane (max_inflight_max null) la finestra non c'è. */}
        {device.max_inflight_max !== null && <NativeSelect label="Window" value={win} onChange={(e) => setWin(e.currentTarget.value)} disabled={mode === 'LOCAL'} data={windowOptions(device.max_inflight_max)} />}
        <Button onClick={() => setConfirm('pin')}>Pin</Button>
        {device.pinned && (
          <Button variant="default" onClick={() => setConfirm('unpin')}>
            Unpin
          </Button>
        )}
      </Group>
      <Text size="xs" c="dimmed" mt="xs">
        {device.pinned ? 'Pinned: the algorithm leaves this device alone until it is unpinned.' : 'Pin to force a policy: the algorithm then leaves this device alone until it is unpinned.'}
      </Text>

      <Modal opened={confirm !== null} onClose={() => setConfirm(null)} title={confirm === 'pin' ? 'Pin the policy' : 'Unpin'} centered>
        <Stack>
          <Text size="sm">
            {confirm === 'pin'
              ? `Pin ${device.device_id} to ${fmtPolicy(req)}? The device applies it at its next heartbeat, and the algorithm does not change it until you unpin.`
              : `Give ${device.device_id} back to the algorithm? Its policy stays ${fmtPolicy(device.policy)} until the algorithm decides otherwise.`}
          </Text>
          <Group justify="flex-end">
            <Button variant="default" onClick={() => setConfirm(null)} disabled={busy}>
              Cancel
            </Button>
            <Button onClick={() => confirm !== null && run(confirm)} loading={busy}>
              Confirm
            </Button>
          </Group>
        </Stack>
      </Modal>
    </Paper>
  )
}

// pinRequest: il corpo del pin. win '' = nessun max_inflight (il massimo).
function pinRequest(mode: Mode, split: string, win: string): SetPolicyRequest {
  if (mode === 'LOCAL') {
    return { mode, split_point: null }
  }
  return { mode, split_point: Number(split), ...(win === '' ? {} : { max_inflight: Number(win) }) }
}

// Gli split validi: da -1 (l'input grezzo, tutto il modello sul server) a
// last_offloading_layer - 1 (solo l'ultimo segmento sul server).
function splitOptions(lastOffloadingLayer: number) {
  return Array.from({ length: lastOffloadingLayer + 1 }, (_, i) => {
    const k = i - 1
    return { value: String(k), label: k === -1 ? '@-1 (raw input)' : `@${k}` }
  })
}

function windowOptions(max: number) {
  return [{ value: '', label: `max (${max})` }, ...Array.from({ length: max }, (_, i) => ({ value: String(i + 1), label: String(i + 1) }))]
}
