import { describe, expect, it } from 'vitest';
import type { FileStatus, PieceStatus, Torrent } from '../types/api';
import { fileCoverage } from '../components/detail/file-coverage';
import { pieceVisualState } from '../components/detail/piece-state';
import { sortTorrents } from '../queries/sort';
import { formatDate } from '../utils/format';

const piece = (overrides: Partial<PieceStatus> = {}): PieceStatus => ({ index: 0, known: true, complete: false, partial: false, wanted: true, checking: false, ...overrides });

const torrent = (overrides: Partial<Torrent> = {}): Torrent => ({ id: 'a', info_hash: 'a', name: 'alpha', state: 'downloading', total_bytes: 10, completed_bytes: 0, progress: 0, created_at: '2026-01-01T00:00:00Z', ...overrides });

describe('piece state encoding', () => {
  it('uses checking, complete, partial, known incomplete, then unknown precedence', () => {
    expect(pieceVisualState(piece({ checking: true, complete: true }))).toBe('checking');
    expect(pieceVisualState(piece({ complete: true, partial: true }))).toBe('complete');
    expect(pieceVisualState(piece({ partial: true }))).toBe('partial');
    expect(pieceVisualState(piece({ known: false }))).toBe('unknown');
    expect(pieceVisualState(piece())).toBe('incomplete');
  });
});

describe('file coverage', () => {
  it('does not invent byte precision and handles zero-size ranges', () => {
    const zero: FileStatus = { path: 'empty', size: 0, piece_start: 2, piece_end: 2 };
    expect(fileCoverage(zero, [])).toEqual({ label: 'Complete · zero bytes', tone: 'complete' });
    expect(fileCoverage({ path: 'x', size: 10, piece_start: 0, piece_end: 2 }, [piece({ index: 0, complete: true }), piece({ index: 1, partial: true })])).toEqual({ label: '1 complete · 1 partial', tone: 'partial' });
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
