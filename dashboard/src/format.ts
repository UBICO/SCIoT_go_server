import type { Policy } from './api/types'

// Formattazione dei valori mostrati. "–" per un valore assente: non è 0.

const DASH = '–'

export function fmtNumber(v: number | null | undefined, digits = 1, unit = ''): string {
  if (v === null || v === undefined || !Number.isFinite(v)) {
    return DASH
  }
  const s = v.toFixed(digits)
  return unit === '' ? s : `${s} ${unit}`
}

export function fmtMs(v: number | null | undefined): string {
  return fmtNumber(v, 1, 'ms')
}

export function fmtPercent(v: number | null | undefined): string {
  return fmtNumber(v, 0, '%')
}

// fmtBytes: memoria in MiB/GiB (potenze di 2, come la riporta il sistema).
export function fmtBytes(v: number | null | undefined): string {
  if (v === null || v === undefined) {
    return DASH
  }
  const mib = v / 2 ** 20
  return mib >= 1024 ? `${(mib / 1024).toFixed(1)} GiB` : `${mib.toFixed(0)} MiB`
}

// fmtFileSize: dimensione di un file, in B, KiB, MiB o GiB.
export function fmtFileSize(bytes: number): string {
  if (bytes < 1024) {
    return `${bytes} B`
  }
  const units = ['KiB', 'MiB', 'GiB']
  let v = bytes / 1024
  let u = 0
  while (v >= 1024 && u < units.length - 1) {
    v /= 1024
    u++
  }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[u]}`
}

// fmtDateTime: data e ora locali.
export function fmtDateTime(iso: string): string {
  return new Date(iso).toLocaleString()
}

// fmtBandwidth: banda in MB/s (potenze di 10, come le reti).
export function fmtBandwidth(bps: number | null | undefined): string {
  return bps === null || bps === undefined ? DASH : fmtNumber(bps / 1e6, 2, 'MB/s')
}

// fmtAge: quanto tempo fa, da secondi. Sotto il minuto in secondi interi.
export function fmtAge(seconds: number): string {
  if (!Number.isFinite(seconds)) {
    return DASH
  }
  const s = Math.max(0, Math.round(seconds))
  if (s < 60) {
    return `${s} s ago`
  }
  if (s < 3600) {
    return `${Math.floor(s / 60)} min ago`
  }
  return `${Math.floor(s / 3600)} h ago`
}

export function fmtDuration(seconds: number): string {
  const s = Math.floor(seconds)
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (d > 0) {
    return `${d}d ${h}h`
  }
  if (h > 0) {
    return `${h}h ${m}m`
  }
  return `${m}m ${s % 60}s`
}

// fmtPolicy: "LOCAL", "SPLIT @5", "SPLIT @5 ×3" (con la finestra).
export function fmtPolicy(p: Pick<Policy, 'mode' | 'split_point' | 'max_inflight'>): string {
  if (p.mode === 'LOCAL') {
    return 'LOCAL'
  }
  const w = p.max_inflight === undefined ? '' : ` ×${p.max_inflight}`
  return `SPLIT @${p.split_point ?? '?'}${w}`
}

export function fmtTime(iso: string): string {
  return new Date(iso).toLocaleTimeString()
}
