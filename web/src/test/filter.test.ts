import { describe, expect, it } from 'vitest';
import { filterTorrents, matchesTorrentFilter, summarizeTorrents } from '../queries/filter';
import type { Torrent } from '../types/api';

function torrent(overrides: Partial<Torrent> = {}): Torrent {
  return {
    id: 'id',
    info_hash: 'ABC123',
    name: 'Ubuntu image',
    state: 'downloading',
    total_bytes: 100,
    completed_bytes: 25,
    progress: 0.25,
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

  it('uses the existing torrent states for status filters', () => {
    expect(matchesTorrentFilter(torrent({ state: 'adding' }), 'downloading')).toBe(true);
    expect(matchesTorrentFilter(torrent({ state: 'seeding', progress: 1 }), 'completed')).toBe(true);
    expect(matchesTorrentFilter(torrent({ state: 'delete_failed' }), 'error')).toBe(true);
  });

  it('keeps summary counts aligned with the filter predicates', () => {
    const torrents = [
      torrent({ id: 'downloading' }),
      torrent({ id: 'seeding', state: 'seeding', progress: 1 }),
      torrent({ id: 'complete', state: 'downloading', progress: 1 }),
      torrent({ id: 'error', state: 'error' }),
    ];
    expect(summarizeTorrents(torrents)).toEqual({ total: 4, downloading: 2, seeding: 1, completed: 2, error: 1 });
  });
});
