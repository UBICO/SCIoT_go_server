import { describe, expect, it } from 'vitest'
import { sampleOf } from '../live/state'
import { device } from '../test/fixtures'
import { type SeriesSpec, toColumns } from './chartData'

describe('toColumns', () => {
  it('builds aligned columns with null for missing values', () => {
    const a = sampleOf(device({ last_seen: '2026-11-19T12:00:00Z' }))!
    const b = sampleOf(device({ last_seen: '2026-11-19T12:00:01Z', telemetry: { inference_count: 0 } }))!
    const series: SeriesSpec[] = [
      { label: 'inference', color: 'blue', value: (s) => s.inferenceMs },
      { label: 'split', color: 'red', value: (s) => s.split },
    ]
    expect(toColumns([a, b], series)).toEqual([
      [a.t, b.t],
      [40.5, null],
      [5, 5],
    ])
  })

  it('is empty without samples', () => {
    expect(toColumns([], [{ label: 'x', color: 'blue', value: (s) => s.rttMs }])).toEqual([[], []])
  })
})
