import type { AdminDecision, AdminDevice, ServerStatus } from '../api/types'

// Lo stato live della dashboard: quello che arriva dallo stream
// /api/v1/admin/events (docs/ADMIN_API.md), più la storia per i grafici.
//
// La storia vive solo nel browser: il server manda sempre e solo l'ultimo
// stato di ogni device (nessuno stato in più nel server, nessuna metrica
// nuova). Quindi dopo un reload i grafici ripartono vuoti, e mostrano solo
// i minuti da quando la pagina è aperta.

// HISTORY_SECONDS: quanto tenere nei grafici. 10 minuti a un heartbeat al
// secondo = 600 punti per device: niente per il browser e per uPlot.
export const HISTORY_SECONDS = 600

// MAX_DECISIONS: i cambi di policy tenuti per la tabella del device.
export const MAX_DECISIONS = 20

// LOCAL_LEVEL: il valore di "split" di un campione in LOCAL, per il
// grafico a gradini della policy. -2 perché gli split validi partono da -1
// (l'input grezzo, tutto il modello sul server: PROTOCOL.md).
export const LOCAL_LEVEL = -2

// Sample è un heartbeat, ridotto ai valori dei grafici. null = il device non
// l'ha mandato (assente ≠ 0).
export interface Sample {
  t: number // last_seen, secondi Unix (l'asse x di uPlot)
  inferenceMs: number | null
  inferenceMaxMs: number | null
  answerMs: number | null
  rttMs: number | null
  cpuPercent: number | null
  bandwidthMBps: number | null // rete misurata dal server, MB/s
  networkLatencyMs: number | null
  split: number // split point della policy del server, LOCAL_LEVEL in LOCAL
}

export interface DeviceLive {
  device: AdminDevice
  samples: Sample[] // in ordine di tempo, al massimo HISTORY_SECONDS
  // I cambi di policy (decisioni con changed), il più recente per primo, al
  // massimo MAX_DECISIONS. Non tutte le decisioni: un algoritmo come
  // "latency" decide a ogni heartbeat, quasi sempre "resta così"; l'ultima
  // decisione, cambio o no, è in device.last_decision.
  changes: AdminDecision[]
}

export interface LiveState {
  status: ServerStatus | null // null fino al primo evento
  devices: Record<string, DeviceLive>
}

export type LiveEvent =
  | { type: 'status'; status: ServerStatus }
  | { type: 'device'; device: AdminDevice }

export const initialLiveState: LiveState = { status: null, devices: {} }

export function sampleOf(d: AdminDevice): Sample | null {
  const tel = d.telemetry
  if (tel === null || d.applied_policy_version === 0) {
    return null // registrato, nessun heartbeat ancora
  }
  const net = d.network
  return {
    t: Date.parse(d.last_seen) / 1000,
    inferenceMs: tel.inference_time_ms ?? null,
    inferenceMaxMs: tel.inference_time_max_ms ?? null,
    answerMs: tel.answer_ms ?? null,
    rttMs: tel.rtt_ms ?? null,
    cpuPercent: tel.cpu_load === undefined ? null : tel.cpu_load * 100,
    bandwidthMBps: net?.bandwidth_bps === undefined ? null : net.bandwidth_bps / 1e6,
    networkLatencyMs: net?.latency_ms ?? null,
    split: d.policy.mode === 'LOCAL' ? LOCAL_LEVEL : (d.policy.split_point ?? LOCAL_LEVEL),
  }
}

// nextSamples aggiunge l'heartbeat di d se è nuovo. Lo stream rimanda un
// device anche per un pin o una decisione, e dopo una riconnessione rimanda
// tutto: un campione si aggiunge solo quando last_seen cambia.
function nextSamples(prev: DeviceLive | undefined, d: AdminDevice): Sample[] {
  const samples = prev?.samples ?? []
  if (prev !== undefined && prev.device.last_seen === d.last_seen) {
    return samples
  }
  const s = sampleOf(d)
  if (s === null) {
    return samples
  }
  const from = s.t - HISTORY_SECONDS
  return [...samples.filter((x) => x.t > from), s]
}

// nextChanges aggiunge l'ultima decisione di d se è nuova (at diverso) e
// ha cambiato la policy.
function nextChanges(prev: DeviceLive | undefined, d: AdminDevice): AdminDecision[] {
  const changes = prev?.changes ?? []
  const last = d.last_decision
  if (last === null || !last.decision.changed || changes[0]?.at === last.at) {
    return changes
  }
  return [last, ...changes].slice(0, MAX_DECISIONS)
}

export function liveReducer(state: LiveState, ev: LiveEvent): LiveState {
  switch (ev.type) {
    case 'status':
      return { ...state, status: ev.status }
    case 'device': {
      const d = ev.device
      const prev = state.devices[d.device_id]
      return {
        ...state,
        devices: {
          ...state.devices,
          [d.device_id]: {
            device: d,
            samples: nextSamples(prev, d),
            changes: nextChanges(prev, d),
          },
        },
      }
    }
  }
}
