import { describe, expect, it } from 'vitest';
import type { FileStatus, PieceStatus, Torrent } from '../types/api';
import { fileCoverage } from '../components/detail/file-coverage';
import { pieceVisualState } from '../components/detail/piece-state';
import { DEFAULT_TORRENT_SORT, sortTorrents, type TorrentSort } from '../queries/sort';
import { formatDate } from '../utils/format';

const piece = (overrides: Partial<PieceStatus> = {}): PieceStatus => ({ index: 0, cached: false, cached_bytes: 0, pinned: false, ...overrides });

const torrent = (overrides: Partial<Torrent> = {}): Torrent => ({ id: 'a', info_hash: 'a', name: 'alpha', state: 'ready', total_bytes: 10, downloaded_bytes: 0, uploaded_bytes: 0, cached_bytes: 0, created_at: '2026-01-01T00:00:00Z', ...overrides });

const order = (items: Torrent[], sort?: TorrentSort) => sortTorrents(items, sort).map((item) => item.id);

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
  it('defaults to newest created_at first and keeps invalid dates last', () => {
    const items = [
      torrent({ id: 'old', created_at: '2026-01-01T00:00:00Z' }),
      torrent({ id: 'new', created_at: '2026-01-02T00:00:00Z' }),
      torrent({ id: 'invalid', created_at: '0001-01-01T00:00:00Z' }),
    ];
    expect(order(items)).toEqual(['new', 'old', 'invalid']);
    expect(DEFAULT_TORRENT_SORT).toEqual({ key: 'created_at', direction: 'desc' });
  });

  it('sorts names in both directions with empty names last and stable tie-breakers', () => {
    const items = [
      torrent({ id: 'empty', name: '   ', info_hash: 'z' }),
      torrent({ id: 'same-b', name: 'same', info_hash: 'b' }),
      torrent({ id: 'same-a', name: 'same', info_hash: 'a' }),
      torrent({ id: 'alpha', name: 'Alpha 2' }),
      torrent({ id: 'beta', name: 'alpha 10' }),
    ];
    expect(order(items, { key: 'name', direction: 'asc' })).toEqual(['alpha', 'beta', 'same-a', 'same-b', 'empty']);
    expect(order(items, { key: 'name', direction: 'desc' })).toEqual(['same-a', 'same-b', 'beta', 'alpha', 'empty']);
  });

  it('sorts sizes in both directions while keeping invalid values last', () => {
    const items = [torrent({ id: 'zero', total_bytes: 0 }), torrent({ id: 'large', total_bytes: 20 }), torrent({ id: 'negative', total_bytes: -1 }), torrent({ id: 'nan', total_bytes: Number.NaN })];
    expect(order(items, { key: 'total_bytes', direction: 'asc' })).toEqual(['zero', 'large', 'nan', 'negative']);
    expect(order(items, { key: 'total_bytes', direction: 'desc' })).toEqual(['large', 'zero', 'nan', 'negative']);
  });

  it('uses lifecycle ranks and leaves unknown states after known states', () => {
    const states = ['delete_failed', 'unknown', 'adding', 'error', 'deleting', 'ready'].map((state) => torrent({ id: state, state }));
    expect(order(states, { key: 'state', direction: 'asc' })).toEqual(['adding', 'ready', 'deleting', 'error', 'delete_failed', 'unknown']);
    expect(order(states, { key: 'state', direction: 'desc' })).toEqual(['delete_failed', 'error', 'deleting', 'ready', 'adding', 'unknown']);
  });

  it('does not mutate the source array when sorting', () => {
    const items = [torrent({ id: 'b' }), torrent({ id: 'a' })];
    const original = [...items];
    sortTorrents(items, { key: 'name', direction: 'asc' });
    expect(items).toEqual(original);
  });
});

describe('created dates', () => {
  it('renders Go time.Time zero values as an empty marker', () => {
    expect(formatDate('0001-01-01T00:00:00Z')).toBe('—');
  });
});
