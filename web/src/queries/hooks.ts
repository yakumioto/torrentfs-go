import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { ApiClient } from '../api/client';
import { ApiError } from '../api/errors';
import type { Operation, Torrent, TorrentStatus } from '../types/api';
import { queryKeys } from './keys';
import { retryDelay, shouldRetry } from './retry';
import { sortTorrents } from './sort';

export interface TorrentLiveSummary {
  state: string;
  progress: number;
  completedBytes: number;
  totalBytes: number;
}

export interface TorrentStatusMeta {
  metainfoReady: boolean;
  pieceLength: number;
  pieceCount: number;
}

export function useTorrentList(api: ApiClient, enabled: boolean) {
  return useQuery({
    queryKey: queryKeys.torrents,
    queryFn: ({ signal }) => api.listTorrents(signal),
    enabled,
    select: sortTorrents,
    refetchInterval: 5000,
    refetchIntervalInBackground: false,
    retry: shouldRetry,
    retryDelay,
  });
}

export function useTorrentDetail(api: ApiClient, id: string, enabled: boolean) {
  return useQuery({
    queryKey: queryKeys.torrent(id),
    queryFn: ({ signal }) => api.getTorrent(id, signal),
    enabled: enabled && id !== '',
    retry: shouldRetry,
    retryDelay,
  });
}

export function useTorrentStatus(api: ApiClient, id: string, enabled: boolean) {
  return useQuery({
    queryKey: queryKeys.torrentStatus(id),
    queryFn: ({ signal }) => api.getTorrentStatus(id, signal),
    enabled: enabled && id !== '',
    refetchInterval: 5000,
    refetchIntervalInBackground: false,
    retry: shouldRetry,
    retryDelay,
  });
}

export function useTorrentStatusSlice<T>(
  api: ApiClient,
  id: string,
  enabled: boolean,
  select: (status: TorrentStatus) => T,
) {
  return useQuery({
    queryKey: queryKeys.torrentStatus(id),
    queryFn: ({ signal }) => api.getTorrentStatus(id, signal),
    enabled: enabled && id !== '',
    select,
    refetchInterval: 5000,
    refetchIntervalInBackground: false,
    retry: shouldRetry,
    retryDelay,
  });
}

const selectStatusTorrent = (status: TorrentStatus): Torrent => status.torrent;
const selectLiveSummary = (status: TorrentStatus): TorrentLiveSummary => ({
  state: status.torrent.state,
  progress: status.torrent.progress,
  completedBytes: status.torrent.completed_bytes,
  totalBytes: status.torrent.total_bytes,
});
const selectStatusMeta = (status: TorrentStatus): TorrentStatusMeta => ({
  metainfoReady: status.metainfo_ready,
  pieceLength: status.piece_length,
  pieceCount: status.pieces.length,
});
const selectStatusFiles = (status: TorrentStatus) => status.files;
const selectStatusPieces = (status: TorrentStatus) => status.pieces;

export function useTorrentStatusTorrent(api: ApiClient, id: string, enabled: boolean) {
  return useTorrentStatusSlice(api, id, enabled, selectStatusTorrent);
}

export function useTorrentLiveSummary(api: ApiClient, id: string, enabled: boolean) {
  return useTorrentStatusSlice(api, id, enabled, selectLiveSummary);
}

export function useTorrentStatusMeta(api: ApiClient, id: string, enabled: boolean) {
  return useTorrentStatusSlice(api, id, enabled, selectStatusMeta);
}

export function useTorrentStatusFiles(api: ApiClient, id: string, enabled: boolean) {
  return useTorrentStatusSlice(api, id, enabled, selectStatusFiles);
}

export function useTorrentStatusPieces(api: ApiClient, id: string, enabled: boolean) {
  return useTorrentStatusSlice(api, id, enabled, selectStatusPieces);
}

export function useAddTorrent(api: ApiClient) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ magnetUri, file, signal }: { magnetUri?: string; file?: File; signal?: AbortSignal }) => {
      if (magnetUri !== undefined) {
        return api.addMagnet(magnetUri, signal);
      }
      if (file !== undefined) {
        return api.addTorrent(file, signal);
      }
      throw new Error('Choose a magnet URI or a torrent file.');
    },
    onSuccess: (torrent: Torrent) => {
      queryClient.setQueryData(queryKeys.torrent(torrent.id), torrent);
      void queryClient.invalidateQueries({ queryKey: queryKeys.torrents });
    },
  });
}

export function useDeleteTorrent(api: ApiClient) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, purgeData, signal }: { id: string; purgeData: boolean; signal?: AbortSignal }) =>
      api.deleteTorrent(id, purgeData, signal),
    onSuccess: (operation: Operation) => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.torrents });
      queryClient.setQueryData(queryKeys.operation(operation.operation_id), operation);
    },
  });
}

export function operationRefetchInterval(operation: Operation | undefined, error: unknown): number | false {
  if (error instanceof ApiError && error.status === 404) {
    return false;
  }
  return operation?.state === 'deleting' ? 1500 : false;
}

export function useOperation(api: ApiClient, operationId: string, enabled: boolean) {
  return useQuery({
    queryKey: queryKeys.operation(operationId),
    queryFn: ({ signal }) => api.getOperation(operationId, signal),
    enabled: enabled && operationId !== '',
    refetchInterval: (query) => operationRefetchInterval(query.state.data, query.state.error),
    retry: shouldRetry,
    retryDelay,
  });
}
