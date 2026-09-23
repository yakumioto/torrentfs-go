import { describe, expect, it } from 'vitest';
import { filterTorrents, matchesTorrentFilter, summarizeTorrents } from '../queries/filter';
import type { Torrent } from '../types/api';

function torrent(overrides: Partial<Torrent> = {}): Torrent {
  return {
    id: 'id',
    info_hash: 'ABC123',
    name: 'Ubuntu image',
    state: 'ready',
    total_bytes: 100,
    downloaded_bytes: 0,
    uploaded_bytes: 0,
    cached_bytes: 25,
    created_at: '2026-09-17T00:00:00Z',
    ...overrides,
  };
}

describe('torrent dashboard filters', () => {
  it('searches names and hashes without case sensitivity', () => {
    const torrents = [torrent(), torrent({ id: 'two', name: 'Movie', info_hash: 'def456' })];
    expect(filterTorrents(torrents, 'ubuntu', 'all')).toHaveLength(1);
    expect(filterTorrents(torrents, 'DEF456', 'all')[0]?.name).toBe('Movie');
  });

  it('matches on state alone and never on cache occupancy', () => {
    expect(matchesTorrentFilter(torrent({ state: 'adding' }), 'all')).toBe(true);
    expect(matchesTorrentFilter(torrent({ state: 'ready' }), 'ready')).toBe(true);
    expect(matchesTorrentFilter(torrent({ state: 'error' }), 'error')).toBe(true);
    expect(matchesTorrentFilter(torrent({ state: 'delete_failed' }), 'error')).toBe(true);
    expect(matchesTorrentFilter(torrent({ state: 'adding' }), 'ready')).toBe(false);
    expect(matchesTorrentFilter(torrent({ state: 'ready', cached_bytes: 0 }), 'ready')).toBe(true);
    expect(matchesTorrentFilter(torrent({ state: 'adding', cached_bytes: 100 }), 'ready')).toBe(false);
  });

  it('keeps summary counts aligned with the filter predicates', () => {
    const torrents = [
      torrent({ id: 'ready-1' }),
      torrent({ id: 'ready-2', state: 'ready', cached_bytes: 100 }),
      torrent({ id: 'adding', state: 'adding' }),
      torrent({ id: 'error', state: 'error' }),
    ];
    expect(summarizeTorrents(torrents)).toEqual({ total: 4, ready: 2, error: 1 });
  });
});
