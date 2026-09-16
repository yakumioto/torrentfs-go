import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { ApiClient } from '../api/client';
import { ApiError } from '../api/errors';
import type { Operation, Torrent } from '../types/api';
import { queryKeys } from './keys';
import { retryDelay, shouldRetry } from './retry';
import { sortTorrents } from './sort';

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
