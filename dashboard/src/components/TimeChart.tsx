import { Paper, Text, useComputedColorScheme } from '@mantine/core'
import { useElementSize } from '@mantine/hooks'
import { useEffect, useMemo, useRef } from 'react'
import uPlot from 'uplot'
import 'uplot/dist/uPlot.min.css'
import type { Sample } from '../live/state'
import { INK, type SeriesSpec, toColumns } from './chartData'

interface Props {
  title: string
  samples: Sample[]
  series: SeriesSpec[] // costante di modulo: vedi SeriesSpec
  unit: string // nei valori della legenda; "" = nessuna
  height?: number
  // Etichette dell'asse y e valori della legenda; default: il numero.
  // Costanti di modulo, come series.
  yValues?: (v: number) => string
  // Intervallo dell'asse y da min e max dei dati; default: quello di uPlot.
  yRange?: (u: uPlot, min: number, max: number) => [number, number]
}

// Le opzioni che, se cambiano, richiedono un grafico nuovo (uPlot non le
// cambia su un grafico esistente). Dati e larghezza invece si aggiornano.
interface Built {
  series: SeriesSpec[]
  unit: string
  height: number
  dark: boolean
  yValues: Props['yValues']
  yRange: Props['yRange']
}

// TimeChart: un grafico a linee nel tempo con uPlot (canvas, ~50 KB).
//
// uPlot non è React: si crea su un <div> e si aggiorna con setData/setSize.
// Un solo effetto gestisce tutto: crea il grafico la prima volta (quando la
// larghezza è nota), poi aggiorna i dati, e lo ricrea solo se cambiano le
// opzioni (Built), per esempio il tema chiaro/scuro.
export function TimeChart({ title, samples, series, unit, height = 180, yValues, yRange }: Props) {
  const { ref: sizeRef, width } = useElementSize()
  const holder = useRef<HTMLDivElement>(null)
  const plot = useRef<uPlot | null>(null)
  const built = useRef<Built | null>(null)
  const dark = useComputedColorScheme('light') === 'dark'
  const data = useMemo(() => toColumns(samples, series), [samples, series])
  // Nessuna serie con un valore: per esempio la banda senza data plane.
  const noValues = data.slice(1).every((col) => Array.from(col).every((v) => v === null || v === undefined))

  useEffect(() => {
    const el = holder.current
    if (el === null || width === 0) {
      return // jsdom (i test) e il primo render: larghezza ancora 0
    }
    const b = built.current
    if (plot.current !== null && b !== null && b.series === series && b.unit === unit && b.height === height && b.dark === dark && b.yValues === yValues && b.yRange === yRange) {
      plot.current.setData(data)
      if (plot.current.width !== width) {
        plot.current.setSize({ width, height })
      }
      return
    }
    plot.current?.destroy()
    plot.current = new uPlot(options({ width, height, series, unit, dark, yValues, yRange }), data, el)
    built.current = { series, unit, height, dark, yValues, yRange }
  }, [data, width, height, series, unit, dark, yValues, yRange])

  // Distrugge il grafico quando il componente sparisce.
  useEffect(() => {
    const p = plot
    return () => {
      p.current?.destroy()
      p.current = null
    }
  }, [])

  return (
    <Paper withBorder p="sm">
      <Text size="sm" fw={500} mb={4}>
        {title}
      </Text>
      <div ref={sizeRef}>
        <div ref={holder} />
        {samples.length === 0 ? (
          <Text size="xs" c="dimmed">
            No heartbeat since the dashboard was opened.
          </Text>
        ) : (
          noValues && (
            <Text size="xs" c="dimmed">
              No value in the heartbeats so far.
            </Text>
          )
        )}
      </div>
    </Paper>
  )
}

function options(o: Omit<Built, 'series'> & { width: number; series: SeriesSpec[] }): uPlot.Options {
  const axis = o.dark ? '#c1c2c5' : '#495057'
  const ink = o.dark ? '#e9ecef' : '#212529'
  const yValues = o.yValues
  const fmt = (v: number | null | undefined) => {
    if (v === null || v === undefined) {
      return '–'
    }
    if (yValues !== undefined) {
      return yValues(v)
    }
    return o.unit === '' ? v.toFixed(1) : `${v.toFixed(1)} ${o.unit}`
  }
  return {
    width: o.width,
    height: o.height,
    scales: { x: { time: true }, ...(o.yRange ? { y: { range: o.yRange } } : {}) },
    series: [
      {},
      ...o.series.map((s) => ({
        label: s.label,
        stroke: s.color === INK ? ink : s.color,
        width: 1.5,
        value: (_u: uPlot, v: number | null) => fmt(v),
        ...(s.stepped ? { paths: uPlot.paths.stepped?.({ align: 1 }) } : {}),
      })),
    ],
    axes: [
      { stroke: axis, grid: { show: false }, ticks: { stroke: axis } },
      {
        stroke: axis,
        grid: { show: false },
        ticks: { stroke: axis },
        size: 56, // spazio per "LOCAL" e per numeri a tre cifre
        // Con yValues (valori discreti, lo split) una tacca per ogni intero:
        // passi interi e poco spazio minimo tra le tacche.
        ...(yValues ? { values: (_u: uPlot, vals: number[]) => vals.map(yValues), incrs: [1, 2, 5, 10, 20], space: 14 } : {}),
      },
    ],
  }
}
