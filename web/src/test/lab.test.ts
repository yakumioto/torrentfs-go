import { describe, expect, it } from 'vitest';
import type { LabView } from '../types/api';
import {
  formatPeerSampleInterval,
  formatRelativeTime,
  formatSignedBytes,
  indexLabTorrentStats,
  labModeLabel,
  labModeOption,
  labOverreportedBytes,
  labPendingRealUploadBytes,
  labTorrentStat,
} from '../utils/lab';

function labView(overrides: Partial<LabView> = {}): LabView {
  return {
    settings: { mode: 'inject', rate_bytes_per_second: 2_097_152, multiplier: 0, jitter_percent: 20, cap_bytes: 0 },
    seed: 42,
    announce_log: '/var/log/torrentfs/lab.jsonl',
    peer_sample_interval_seconds: 5,
    totals: {
      real_uploaded_bytes: 5_000_000,
      real_uploaded_at_last_announce_bytes: 1000,
      reported_uploaded_bytes: 4_197_247,
      overreported_bytes: 4_196_247,
      pending_real_upload_bytes: 4_999_000,
    },
    torrents: [
      {
        info_hash: 'abc123',
        real_uploaded_bytes: 5_000_000,
        real_uploaded_at_last_announce_bytes: 1000,
        reported_uploaded_bytes: 4_197_247,
        last_announce_at: '2026-11-05T15:12:00.123456Z',
      },
      {
        info_hash: 'def456',
        real_uploaded_bytes: 2048,
        real_uploaded_at_last_announce_bytes: 2048,
        reported_uploaded_bytes: 1024,
        last_announce_at: null,
      },
    ],
    ...overrides,
  };
}

describe('lab presentation helpers', () => {
  it('labels the three experiment modes in Chinese', () => {
    expect(labModeLabel('baseline')).toContain('基线');
    expect(labModeLabel('inject')).toContain('虚报');
    expect(labModeLabel('amplify')).toContain('放大');
    expect(labModeLabel(undefined)).toBe('—');
    expect(labModeOption('baseline')?.description).toContain('对照组');
  });

  it('compares reported bytes against the counter at the last announce', () => {
    // The live counter is much larger here; using it would show a bogus
    // negative "overreport" even though the multiplier is working.
    expect(labOverreportedBytes(4_197_247, 1000)).toBe(4_196_247);
    expect(labOverreportedBytes(1024, 2048)).toBe(-1024);
    expect(labOverreportedBytes(2048, 2048)).toBe(0);
    expect(formatSignedBytes(4_196_247)).toBe('+4.0 MiB');
    expect(formatSignedBytes(-1024)).toBe('-1.0 KiB');
    expect(formatSignedBytes(0)).toBe('0 B');
    expect(formatSignedBytes(Number.NaN)).toBe('—');
  });

  it('reports the real upload not yet part of an announce', () => {
    expect(labPendingRealUploadBytes(5_000_000, 1000)).toBe(4_999_000);
    expect(labPendingRealUploadBytes(2048, 2048)).toBe(0);
  });

  it('looks up per-torrent counters by info hash', () => {
    const view = labView();
    expect(labTorrentStat(view, 'abc123')?.reported_uploaded_bytes).toBe(4_197_247);
    expect(labTorrentStat(view, 'abc123')?.real_uploaded_at_last_announce_bytes).toBe(1000);
    expect(labTorrentStat(view, 'missing')).toBeUndefined();
    expect(indexLabTorrentStats(view).get('def456')?.real_uploaded_bytes).toBe(2048);

    expect(indexLabTorrentStats(undefined).size).toBe(0);
  });

  it('describes the peer sampling interval', () => {
    expect(formatPeerSampleInterval(5)).toBe('5 秒');
    expect(formatPeerSampleInterval(0)).toBe('—');
    expect(formatPeerSampleInterval(Number.NaN)).toBe('—');
  });

  it('formats the last announce time relative to now', () => {
    const now = Date.parse('2026-11-05T15:25:00Z');
    expect(formatRelativeTime('2026-11-05T15:12:00Z', now)).toBe('13 分钟前');
    expect(formatRelativeTime('2026-11-05T15:24:30Z', now)).toBe('30 秒前');
    expect(formatRelativeTime('2026-11-05T15:24:55Z', now)).toBe('刚刚');
    expect(formatRelativeTime('2026-11-05T12:25:00Z', now)).toBe('3 小时前');
    expect(formatRelativeTime('2026-11-03T15:25:00Z', now)).toBe('2 天前');
    expect(formatRelativeTime(null, now)).toBe('—');
    expect(formatRelativeTime(undefined, now)).toBe('—');
    expect(formatRelativeTime('not-a-date', now)).toBe('—');
  });
});
