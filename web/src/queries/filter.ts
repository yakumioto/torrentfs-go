import type { Torrent } from '../types/api';

export type TorrentFilter = 'all' | 'ready' | 'error' | 'favorite';

export interface TorrentSummary {
  total: number;
  ready: number;
  error: number;
}

export function matchesTorrentSearch(torrent: Torrent, search: string): boolean {
  const normalizedSearch = search.trim().toLocaleLowerCase();
  if (normalizedSearch === '') {
    return true;
  }
  return torrent.name.toLocaleLowerCase().includes(normalizedSearch)
    || torrent.info_hash.toLocaleLowerCase().includes(normalizedSearch);
}

export function matchesTorrentFilter(torrent: Torrent, filter: TorrentFilter): boolean {
  switch (filter) {
    case 'ready':
      return torrent.state === 'ready';
    case 'error':
      return torrent.state === 'error' || torrent.state === 'delete_failed';
    case 'favorite':
      return torrent.favorite === true;
    case 'all':
      return true;
  }
}

export function filterTorrents(torrents: Torrent[], search: string, filter: TorrentFilter): Torrent[] {
  return torrents.filter((torrent) => matchesTorrentSearch(torrent, search) && matchesTorrentFilter(torrent, filter));
}

export function summarizeTorrents(torrents: Torrent[]): TorrentSummary {
  return {
    total: torrents.length,
    ready: torrents.filter((torrent) => matchesTorrentFilter(torrent, 'ready')).length,
    error: torrents.filter((torrent) => matchesTorrentFilter(torrent, 'error')).length,
  };
}
