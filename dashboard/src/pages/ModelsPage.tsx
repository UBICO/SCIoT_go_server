import { Badge, Button, Code, Group, Paper, SimpleGrid, Stack, Text, Title } from '@mantine/core'
import { Link, useLoaderData } from 'react-router'
import type { AdminModel, BuildsResponse } from '../api/types'

// ModelsPage: i modelli del registry (MODELS.md), con quanti device li
// usano. I dati arrivano da modelsLoader (router.tsx).
export function ModelsPage() {
  const { models, builds } = useLoaderData() as { models: AdminModel[]; builds?: BuildsResponse }
  return (
    <>
      <Group justify="space-between" mb="md">
        <Title order={3}>Models</Title>
        {builds?.enabled && (
          <Button component={Link} to="/models/build" size="xs">
            Build a model
          </Button>
        )}
      </Group>
      <Text size="sm" c="dimmed" mb="md">
        The models the server loaded at start. A new model is used after it is added to config.json and the server restarts.
      </Text>
      {models.length === 0 ? (
        <Text c="dimmed">No model in the registry.</Text>
      ) : (
        <SimpleGrid cols={{ base: 1, lg: 2 }}>
          {models.map((m) => (
            <ModelCard key={m.model_hash} model={m} />
          ))}
        </SimpleGrid>
      )}
    </>
  )
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <Group gap="xs" wrap="nowrap" align="flex-start">
      <Text size="sm" c="dimmed" w={130} style={{ flexShrink: 0 }}>
        {label}
      </Text>
      <Text size="sm" component="div">
        {children}
      </Text>
    </Group>
  )
}

function ModelCard({ model: m }: { model: AdminModel }) {
  const pp = m.postprocess
  return (
    <Paper withBorder p="sm">
      <Group justify="space-between" mb="xs">
        <Text fw={500}>{m.model_key}</Text>
        <Badge variant="light" color={m.devices > 0 ? 'blue' : 'gray'}>
          {m.devices} {m.devices === 1 ? 'device' : 'devices'}
        </Badge>
      </Group>
      <Stack gap={4}>
        <Row label="Hash">
          <Code>{m.model_hash}</Code>
        </Row>
        <Row label="Segments">
          {m.num_layers} · splits -1…{m.last_offloading_layer - 1}
        </Row>
        <Row label="Input">
          {m.input.shape.join(' × ')} {m.input.dtype}
        </Row>
        <Row label="Decoder">
          {pp.decoder} · score ≥ {pp.score_threshold}
          {pp.iou_threshold === undefined ? '' : ` · IoU ${pp.iou_threshold}`} · max {pp.max_detections}
        </Row>
        <Row label="Labels">{pp.labels.join(', ')}</Row>
      </Stack>
    </Paper>
  )
}
