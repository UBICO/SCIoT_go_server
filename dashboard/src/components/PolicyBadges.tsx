import { Badge, Group } from '@mantine/core'
import type { AdminDevice } from '../api/types'
import { fmtPolicy } from '../format'

// PolicyBadges: la policy del server per il device, se è fissata
// dall'admin (pinned), e se il device non la applica ancora (la versione
// dell'ultimo heartbeat è più vecchia).
export function PolicyBadges({ device }: { device: AdminDevice }) {
  const p = device.policy
  const pending = device.applied_policy_version !== 0 && device.applied_policy_version < p.policy_version
  return (
    <Group gap={4} wrap="nowrap">
      <Badge color={p.mode === 'LOCAL' ? 'gray' : 'blue'} variant="light">
        {fmtPolicy(p)}
      </Badge>
      {device.pinned && (
        <Badge color="orange" variant="light">
          pinned
        </Badge>
      )}
      {pending && (
        <Badge color="yellow" variant="outline" title={`the device applies v${device.applied_policy_version}, the server has v${p.policy_version}`}>
          pending
        </Badge>
      )}
    </Group>
  )
}
