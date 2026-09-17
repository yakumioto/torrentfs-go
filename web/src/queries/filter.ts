import type { Torrent } from '../types/api';

export type TorrentFilter = 'all' | 'downloading' | 'seeding' | 'completed' | 'error';

export interface TorrentSummary {
  total: number;
  downloading: number;
  seeding: number;
  completed: number;
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
    case 'downloading':
      return torrent.state === 'adding' || torrent.state === 'downloading';
    case 'seeding':
      return torrent.state === 'seeding';
    case 'completed':
      return torrent.progress >= 1 || torrent.state === 'seeding';
    case 'error':
      return torrent.state === 'error' || torrent.state === 'delete_failed';
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
    downloading: torrents.filter((torrent) => matchesTorrentFilter(torrent, 'downloading')).length,
    seeding: torrents.filter((torrent) => matchesTorrentFilter(torrent, 'seeding')).length,
    completed: torrents.filter((torrent) => matchesTorrentFilter(torrent, 'completed')).length,
    error: torrents.filter((torrent) => matchesTorrentFilter(torrent, 'error')).length,
  };
}
