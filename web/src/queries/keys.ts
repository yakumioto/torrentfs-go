export const queryKeys = {
  torrents: ['torrents'] as const,
  torrent: (id: string) => ['torrent', id] as const,
  torrentStatus: (id: string) => ['torrent-status', id] as const,
  operation: (operationId: string) => ['operation', operationId] as const,
};
