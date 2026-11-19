import type uPlot from 'uplot'
import type { Sample } from '../live/state'

// INK: il colore del testo, nero sul tema chiaro e quasi bianco sullo
// scuro (un nero fisso sparirebbe sullo sfondo scuro). TimeChart lo risolve.
export const INK = 'ink'

// SeriesSpec è una linea di un grafico: cosa leggere da un campione e come
// disegnarlo. Le liste di SeriesSpec vanno dichiarate fuori dai componenti
// (costanti di modulo): TimeChart ricrea il grafico quando cambia l'identità
// della lista.
export interface SeriesSpec {
  label: string
  color: string // un colore CSS, o INK
  value: (s: Sample) => number | null
  stepped?: boolean // a gradini: per valori discreti (lo split point)
}

// toColumns mette i campioni nel formato di uPlot: [x, y1, y2, …], colonne
// della stessa lunghezza. null = buco nella linea (valore non mandato).
export function toColumns(samples: Sample[], series: SeriesSpec[]): uPlot.AlignedData {
  return [samples.map((s) => s.t), ...series.map((spec) => samples.map(spec.value))]
}
