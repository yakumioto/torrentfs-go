import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { ApiClient } from '../api/client';
import { ApiError } from '../api/errors';
import type { Operation, PruneResult, RuntimeStats, SubtitleUploadResponse, Torrent, TorrentStatus } from '../types/api';
import { queryKeys } from './keys';
import { retryDelay, shouldRetry } from './retry';
import { sortTorrents } from './sort';

export interface TorrentLiveSummary {
  state: string;
  cachedBytes: number;
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

export function useRuntimeStats(api: ApiClient, enabled: boolean) {
  return useQuery<RuntimeStats>({
    queryKey: queryKeys.runtimeStats,
    queryFn: ({ signal }) => api.getRuntimeStats(signal),
    enabled,
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

function torrentStatusQueryOptions(api: ApiClient, id: string, enabled: boolean) {
  return {
    queryKey: queryKeys.torrentStatus(id),
    queryFn: ({ signal }: { signal: AbortSignal }) => api.getTorrentStatus(id, signal),
    enabled: enabled && id !== '',
    refetchInterval: 5000,
    refetchIntervalInBackground: false,
    retry: shouldRetry,
    retryDelay,
  };
}

export function useTorrentStatus(api: ApiClient, id: string, enabled: boolean) {
  return useQuery(torrentStatusQueryOptions(api, id, enabled));
}

export function useTorrentStatusSlice<T>(
  api: ApiClient,
  id: string,
  enabled: boolean,
  select: (status: TorrentStatus) => T,
) {
  return useQuery({ ...torrentStatusQueryOptions(api, id, enabled), select });
}

const selectStatusTorrent = (status: TorrentStatus): Torrent => status.torrent;
const selectLiveSummary = (status: TorrentStatus): TorrentLiveSummary => ({
  state: status.torrent.state,
  cachedBytes: status.torrent.cached_bytes,
  totalBytes: status.torrent.total_bytes,
});
const selectStatusMeta = (status: TorrentStatus): TorrentStatusMeta => ({
  metainfoReady: status.metainfo_ready,
  pieceLength: status.piece_length,
  pieceCount: status.pieces.length,
});
const selectStatusFiles = (status: TorrentStatus) => status.files;
const selectStatusPieces = (status: TorrentStatus) => status.pieces;
const selectStatusSubtitles = (status: TorrentStatus) => status.subtitles;
const selectStatusSubtitleTargets = (status: TorrentStatus) => status.subtitle_targets;

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

export function useTorrentStatusSubtitles(api: ApiClient, id: string, enabled: boolean) {
  return useTorrentStatusSlice(api, id, enabled, selectStatusSubtitles);
}

export function useTorrentSubtitleTargets(api: ApiClient, id: string, enabled: boolean) {
  return useTorrentStatusSlice(api, id, enabled, selectStatusSubtitleTargets);
}

export function useUploadSubtitle(api: ApiClient) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, videoPath, file, signal }: { id: string; videoPath: string; file: File; signal?: AbortSignal }) =>
      api.uploadSubtitle(id, videoPath, file, signal),
    onSuccess: (_result: SubtitleUploadResponse, variables) => {
      // Only this torrent's status carries the subtitle list and targets, so
      // refreshing global statistics here would be unrelated churn.
      void queryClient.invalidateQueries({ queryKey: queryKeys.torrentStatus(variables.id) });
    },
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

export type TorrentFileBatchStatus = 'uploading' | 'succeeded' | 'failed' | 'skipped';

export interface TorrentFileBatchUpdate {
  file: File;
  index: number;
  total: number;
  status: TorrentFileBatchStatus;
  torrent?: Torrent;
  error?: unknown;
}

export interface TorrentFileBatchItemResult {
  file: File;
  index: number;
  status: Exclude<TorrentFileBatchStatus, 'uploading'>;
  torrent?: Torrent;
  error?: unknown;
}

export interface TorrentFileBatchResult {
  items: TorrentFileBatchItemResult[];
  torrents: Torrent[];
  stopped: boolean;
}

export interface AddTorrentFilesVariables {
  files: File[];
  onFileStatus?: (update: TorrentFileBatchUpdate) => void;
}

function shouldContinueTorrentFileBatch(error: unknown): boolean {
  return error instanceof ApiError && [400, 409, 413, 415].includes(error.status);
}

export function useAddTorrentFiles(api: ApiClient) {
  const queryClient = useQueryClient();
  return useMutation<TorrentFileBatchResult, Error, AddTorrentFilesVariables>({
    mutationFn: async ({ files, onFileStatus }) => {
      const items: TorrentFileBatchItemResult[] = [];
      const torrents = new Map<string, Torrent>();
      let stopped = false;

      for (const [index, file] of files.entries()) {
        onFileStatus?.({ file, index, total: files.length, status: 'uploading' });
        try {
          const torrent = await api.addTorrent(file);
          items.push({ file, index, status: 'succeeded', torrent });
          torrents.set(torrent.id, torrent);
          onFileStatus?.({ file, index, total: files.length, status: 'succeeded', torrent });
        } catch (error) {
          items.push({ file, index, status: 'failed', error });
          onFileStatus?.({ file, index, total: files.length, status: 'failed', error });
          if (shouldContinueTorrentFileBatch(error)) {
            continue;
          }

          stopped = true;
          for (let skippedIndex = index + 1; skippedIndex < files.length; skippedIndex += 1) {
            const skippedFile = files[skippedIndex];
            items.push({ file: skippedFile, index: skippedIndex, status: 'skipped', error });
            onFileStatus?.({ file: skippedFile, index: skippedIndex, total: files.length, status: 'skipped', error });
          }
          break;
        }
      }

      return { items, torrents: [...torrents.values()], stopped };
    },
    onSuccess: (result) => {
      for (const torrent of result.torrents) {
        queryClient.setQueryData(queryKeys.torrent(torrent.id), torrent);
      }
      if (result.torrents.length > 0) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.torrents });
      }
    },
  });
}

export function useDeleteTorrent(api: ApiClient) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, signal }: { id: string; signal?: AbortSignal }) =>
      api.deleteTorrent(id, signal),
    onSuccess: (operation: Operation) => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.torrents });
      queryClient.setQueryData(queryKeys.operation(operation.operation_id), operation);
    },
  });
}

export function useSetFavorite(api: ApiClient) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, favorite, signal }: { id: string; favorite: boolean; signal?: AbortSignal }) =>
      api.setFavorite(id, favorite, signal),
    onSuccess: (torrent: Torrent) => {
      queryClient.setQueryData(queryKeys.torrent(torrent.id), torrent);
      void queryClient.invalidateQueries({ queryKey: queryKeys.torrents });
    },
  });
}

export function usePruneTorrents(api: ApiClient) {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ olderThanDays, signal }: { olderThanDays: number; signal?: AbortSignal }) =>
      api.pruneTorrents(olderThanDays, signal),
    onSuccess: (result: PruneResult) => {
      for (const operation of result.operations) {
        queryClient.setQueryData(queryKeys.operation(operation.operation_id), operation);
      }
      void queryClient.invalidateQueries({ queryKey: queryKeys.torrents });
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
