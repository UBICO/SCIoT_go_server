import { describe, expect, it } from 'vitest'
import { fmtAge, fmtBandwidth, fmtBytes, fmtDuration, fmtFileSize, fmtMs, fmtNumber, fmtPercent, fmtPolicy } from './format'

describe('format', () => {
  it('shows a dash for missing values, not 0', () => {
    expect(fmtMs(undefined)).toBe('–')
    expect(fmtMs(null)).toBe('–')
    expect(fmtNumber(Number.POSITIVE_INFINITY)).toBe('–')
    expect(fmtBandwidth(undefined)).toBe('–')
    expect(fmtBytes(undefined)).toBe('–')
    expect(fmtMs(0)).toBe('0.0 ms')
  })

  it('formats units', () => {
    expect(fmtMs(12.345)).toBe('12.3 ms')
    expect(fmtPercent(41.6)).toBe('42 %')
    expect(fmtBandwidth(11_500_000)).toBe('11.50 MB/s')
    expect(fmtBytes(812_345_344)).toBe('775 MiB')
    expect(fmtBytes(3 * 2 ** 30)).toBe('3.0 GiB')
  })

  it('formats file sizes', () => {
    expect(fmtFileSize(0)).toBe('0 B')
    expect(fmtFileSize(1023)).toBe('1023 B')
    expect(fmtFileSize(1536)).toBe('1.5 KiB')
    expect(fmtFileSize(2_500_000)).toBe('2.4 MiB')
    expect(fmtFileSize(50 * 2 ** 20)).toBe('50 MiB')
    expect(fmtFileSize(3 * 2 ** 30)).toBe('3.0 GiB')
  })

  it('formats ages and durations', () => {
    expect(fmtAge(-0.4)).toBe('0 s ago') // orologi leggermente diversi
    expect(fmtAge(42.4)).toBe('42 s ago')
    expect(fmtAge(125)).toBe('2 min ago')
    expect(fmtAge(7300)).toBe('2 h ago')
    expect(fmtDuration(65)).toBe('1m 5s')
    expect(fmtDuration(3725)).toBe('1h 2m')
    expect(fmtDuration(90000)).toBe('1d 1h')
  })

  it('formats policies', () => {
    expect(fmtPolicy({ mode: 'LOCAL', split_point: null })).toBe('LOCAL')
    expect(fmtPolicy({ mode: 'SPLIT', split_point: 0 })).toBe('SPLIT @0')
    expect(fmtPolicy({ mode: 'SPLIT', split_point: 5, max_inflight: 3 })).toBe('SPLIT @5 ×3')
  })
})
