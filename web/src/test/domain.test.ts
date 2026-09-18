import { describe, expect, it } from 'vitest';
import type { FileStatus, PieceStatus, Torrent } from '../types/api';
import { fileCoverage } from '../components/detail/file-coverage';
import { pieceVisualState } from '../components/detail/piece-state';
import { sortTorrents } from '../queries/sort';
import { formatDate } from '../utils/format';

const piece = (overrides: Partial<PieceStatus> = {}): PieceStatus => ({ index: 0, cached: false, cached_bytes: 0, pinned: false, ...overrides });

const torrent = (overrides: Partial<Torrent> = {}): Torrent => ({ id: 'a', info_hash: 'a', name: 'alpha', state: 'ready', total_bytes: 10, cached_bytes: 0, created_at: '2026-01-01T00:00:00Z', ...overrides });

describe('piece cache state encoding', () => {
  it('ranks pinned ahead of cached ahead of uncached', () => {
    expect(pieceVisualState(piece({ pinned: true, cached: true }))).toBe('pinned');
    expect(pieceVisualState(piece({ pinned: true }))).toBe('pinned');
    expect(pieceVisualState(piece({ cached: true }))).toBe('cached');
    expect(pieceVisualState(piece())).toBe('uncached');
  });
});

describe('file coverage', () => {
  it('counts cached pieces and handles zero-size ranges', () => {
    const zero: FileStatus = { path: 'empty', size: 0, piece_start: 2, piece_end: 2 };
    expect(fileCoverage(zero, [])).toEqual({ label: '零字节文件', tone: 'cached' });
    expect(fileCoverage({ path: 'x', size: 10, piece_start: 0, piece_end: 2 }, [piece({ index: 0, cached: true }), piece({ index: 1 })])).toEqual({ label: '已缓存 1 / 2 个数据块', tone: 'partial' });
    expect(fileCoverage({ path: 'y', size: 10, piece_start: 0, piece_end: 2 }, [piece({ index: 0, cached: true }), piece({ index: 1, cached: true })])).toEqual({ label: '已缓存', tone: 'cached' });
    expect(fileCoverage({ path: 'z', size: 10, piece_start: 0, piece_end: 2 }, [piece({ index: 0 }), piece({ index: 1 })])).toEqual({ label: '已缓存 0 / 2 个数据块', tone: 'uncached' });
  });
});

describe('torrent sorting', () => {
  it('sorts by name, puts invalid dates after valid dates, then id', () => {
    expect(sortTorrents([torrent({ id: 'b', name: 'Beta' }), torrent({ id: 'a', name: 'alpha' }), torrent({ id: 'z', name: 'alpha', created_at: '0001-01-01T00:00:00Z' })]).map((item) => item.id)).toEqual(['a', 'z', 'b']);
  });
});

describe('created dates', () => {
  it('renders Go time.Time zero values as an empty marker', () => {
    expect(formatDate('0001-01-01T00:00:00Z')).toBe('—');
  });
});
