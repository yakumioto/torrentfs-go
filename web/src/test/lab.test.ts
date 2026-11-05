import { describe, expect, it } from 'vitest';
import type { LabView } from '../types/api';
import {
  formatPeerSampleInterval,
  formatSignedBytes,
  indexLabTorrentStats,
  labModeLabel,
  labModeOption,
  labOverreportedBytes,
  labTorrentStat,
} from '../utils/lab';

function labView(overrides: Partial<LabView> = {}): LabView {
  return {
    enabled: true,
    settings: { mode: 'inject', rate_bytes_per_second: 2_097_152, multiplier: 0, jitter_percent: 20, cap_bytes: 0 },
    seed: 42,
    announce_log: '/var/log/torrentfs/lab.jsonl',
    peer_sample_interval_seconds: 5,
    totals: { real_uploaded_bytes: 1000, reported_uploaded_bytes: 4_197_247, overreported_bytes: 4_196_247 },
    torrents: [
      { info_hash: 'abc123', real_uploaded_bytes: 1000, reported_uploaded_bytes: 4_197_247 },
      { info_hash: 'def456', real_uploaded_bytes: 2048, reported_uploaded_bytes: 1024 },
    ],
    ...overrides,
  };
}

describe('lab presentation helpers', () => {
  it('labels the three experiment modes in Chinese', () => {
    expect(labModeLabel('baseline')).toContain('基线');
    expect(labModeLabel('inject')).toContain('虚报');
    expect(labModeLabel('amplify')).toContain('放大');
    expect(labModeLabel(undefined)).toBe('未启用');
    expect(labModeOption('baseline')?.description).toContain('对照组');
  });

  it('formats overreported bytes with an explicit sign', () => {
    expect(labOverreportedBytes(4_197_247, 1000)).toBe(4_196_247);
    expect(labOverreportedBytes(1024, 2048)).toBe(-1024);
    expect(formatSignedBytes(4_196_247)).toBe('+4.0 MiB');
    expect(formatSignedBytes(-1024)).toBe('-1.0 KiB');
    expect(formatSignedBytes(0)).toBe('0 B');
    expect(formatSignedBytes(Number.NaN)).toBe('—');
  });

  it('looks up per-torrent counters by info hash and ignores the disabled view', () => {
    const view = labView();
    expect(labTorrentStat(view, 'abc123')?.reported_uploaded_bytes).toBe(4_197_247);
    expect(labTorrentStat(view, 'missing')).toBeUndefined();
    expect(indexLabTorrentStats(view).get('def456')?.real_uploaded_bytes).toBe(2048);

    const disabled = labView({ enabled: false, settings: null, torrents: [] });
    expect(labTorrentStat(disabled, 'abc123')).toBeUndefined();
    expect(indexLabTorrentStats(disabled).size).toBe(0);
    expect(indexLabTorrentStats(undefined).size).toBe(0);
  });

  it('describes the peer sampling interval', () => {
    expect(formatPeerSampleInterval(5)).toBe('5 秒');
    expect(formatPeerSampleInterval(0)).toBe('—');
    expect(formatPeerSampleInterval(Number.NaN)).toBe('—');
  });
});
