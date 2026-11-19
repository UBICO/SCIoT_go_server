import { Code, Text } from '@mantine/core'
import { structuredPatch } from 'diff'

// ConfigDiff: le differenze tra il file sul server (before) e il testo
// modificato (after), come un diff unificato: righe tolte in rosso, aggiunte
// in verde, 3 righe di contesto attorno a ogni cambiamento. Il resto del file
// non si mostra.
export function ConfigDiff({ before, after }: { before: string; after: string }) {
  const { hunks } = structuredPatch('config.json', 'config.json', before, after, undefined, undefined, { context: 3 })
  if (hunks.length === 0) {
    return (
      <Text size="sm" c="dimmed">
        No change.
      </Text>
    )
  }
  return (
    // overflowX: le righe lunghe (percorsi) scorrono invece di uscire dal riquadro.
    <Code block style={{ overflowX: 'auto' }}>
      {hunks.map((h) => (
        <div key={`${h.oldStart}-${h.newStart}`}>
          <div style={{ color: 'var(--mantine-color-dimmed)' }}>{`@@ -${h.oldStart},${h.oldLines} +${h.newStart},${h.newLines} @@`}</div>
          {h.lines.map((line, i) => (
            <div key={i} style={{ color: lineColor(line), whiteSpace: 'pre' }}>
              {line}
            </div>
          ))}
        </div>
      ))}
    </Code>
  )
}

function lineColor(line: string): string | undefined {
  if (line.startsWith('+')) {
    return 'var(--mantine-color-green-text)'
  }
  if (line.startsWith('-')) {
    return 'var(--mantine-color-red-text)'
  }
  return undefined
}
