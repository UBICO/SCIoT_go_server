import { describe, expect, it } from 'vitest'
import { decision, device, liveState, status } from '../test/fixtures'
import { HISTORY_SECONDS, initialLiveState, LOCAL_LEVEL, liveReducer, MAX_DECISIONS, sampleOf } from './state'

// last_seen a t secondi da un'origine fissa.
const at = (t: number) => new Date(Date.UTC(2026, 10, 19, 12, 0, 0) + t * 1000).toISOString()

describe('sampleOf', () => {
  it('reads the heartbeat', () => {
    const s = sampleOf(device({ last_seen: at(10) }))
    expect(s).toEqual({
      t: Date.parse(at(10)) / 1000,
      inferenceMs: 40.5,
      inferenceMaxMs: 61.2,
      answerMs: 9.3,
      rttMs: 12.1,
      cpuPercent: 42,
      bandwidthMBps: 11,
      networkLatencyMs: 2.5,
      split: 5,
    })
  })

  it('has no sample before the first heartbeat', () => {
    expect(sampleOf(device({ applied_policy_version: 0, telemetry: null }))).toBeNull()
  })

  it('keeps missing values as null, not 0', () => {
    const s = sampleOf(device({ telemetry: { inference_count: 0 }, network: null }))
    expect(s).toMatchObject({ inferenceMs: null, cpuPercent: null, bandwidthMBps: null, networkLatencyMs: null })
  })

  it('maps LOCAL to LOCAL_LEVEL and keeps the splits -1 and 0', () => {
    expect(sampleOf(device({ policy: { mode: 'LOCAL', split_point: null, policy_version: 3 } }))?.split).toBe(LOCAL_LEVEL)
    expect(sampleOf(device({ policy: { mode: 'SPLIT', split_point: -1, policy_version: 3 } }))?.split).toBe(-1)
    expect(sampleOf(device({ policy: { mode: 'SPLIT', split_point: 0, policy_version: 3 } }))?.split).toBe(0)
    expect(LOCAL_LEVEL).toBeLessThan(-1)
  })
})

describe('liveReducer', () => {
  it('stores the status', () => {
    const st = status({ devices: 4 })
    expect(liveReducer(initialLiveState, { type: 'status', status: st }).status).toBe(st)
  })

  it('adds a sample per new heartbeat only', () => {
    let s = liveState([device({ last_seen: at(0) })])
    expect(s.devices.pi_a?.samples).toHaveLength(1)

    // Stesso last_seen: un pin, una decisione o una riconnessione.
    s = liveReducer(s, { type: 'device', device: device({ last_seen: at(0), pinned: true }) })
    expect(s.devices.pi_a?.samples).toHaveLength(1)
    expect(s.devices.pi_a?.device.pinned).toBe(true)

    s = liveReducer(s, { type: 'device', device: device({ last_seen: at(1) }) })
    expect(s.devices.pi_a?.samples.map((x) => x.t)).toEqual([at(0), at(1)].map((x) => Date.parse(x) / 1000))
  })

  it('drops samples older than HISTORY_SECONDS', () => {
    let s = liveState([device({ last_seen: at(0) }), device({ last_seen: at(1) })])
    s = liveReducer(s, { type: 'device', device: device({ last_seen: at(HISTORY_SECONDS + 0.5) }) })
    expect(s.devices.pi_a?.samples).toHaveLength(2) // at(0) è fuori, at(1) no
  })

  it('keeps devices apart', () => {
    const s = liveState([device(), device({ device_id: 'pi_b' })])
    expect(Object.keys(s.devices).sort()).toEqual(['pi_a', 'pi_b'])
  })

  it('collects the policy changes, newest first, at most MAX_DECISIONS', () => {
    let s = liveState([device({ last_decision: decision(at(0)) })])
    s = liveReducer(s, { type: 'device', device: device({ last_seen: at(1), last_decision: decision(at(0)) }) })
    expect(s.devices.pi_a?.changes).toHaveLength(1) // stessa decisione

    for (let i = 1; i <= MAX_DECISIONS + 5; i++) {
      s = liveReducer(s, { type: 'device', device: device({ last_decision: decision(at(i)) }) })
    }
    const cs = s.devices.pi_a?.changes ?? []
    expect(cs).toHaveLength(MAX_DECISIONS)
    expect(cs[0]?.at).toBe(at(MAX_DECISIONS + 5))
  })

  it('skips the decisions that keep the policy', () => {
    let s = liveState([device({ last_decision: decision(at(0)) })])
    s = liveReducer(s, { type: 'device', device: device({ last_decision: decision(at(1), { changed: false, reason: 'best' }) }) })
    expect(s.devices.pi_a?.changes.map((c) => c.at)).toEqual([at(0)])
    expect(s.devices.pi_a?.device.last_decision?.decision.reason).toBe('best')
  })

  it('keeps the history after a new registration', () => {
    let s = liveState([device({ last_seen: at(0) })])
    s = liveReducer(s, { type: 'device', device: device({ session_id: 'NEW', applied_policy_version: 0, telemetry: null, last_seen: at(5), last_decision: null }) })
    expect(s.devices.pi_a?.samples).toHaveLength(1)
    expect(s.devices.pi_a?.device.session_id).toBe('NEW')
  })
})
