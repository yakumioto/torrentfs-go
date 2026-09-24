import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from '../api/client';
import { ApiError } from '../api/errors';
import { operationRefetchInterval, useAddTorrentFiles, useOperation } from '../queries/hooks';
import { queryKeys } from '../queries/keys';
import { retryDelay, shouldRetry } from '../queries/retry';
import type { Operation } from '../types/api';

describe('query contracts', () => {
  it('keeps the planned cache key shapes', () => {
    expect(queryKeys.torrents).toEqual(['torrents']);
    expect(queryKeys.runtimeStats).toEqual(['runtime-stats']);
    expect(queryKeys.torrent('abc')).toEqual(['torrent', 'abc']);
    expect(queryKeys.torrentStatus('abc')).toEqual(['torrent-status', 'abc']);
    expect(queryKeys.operation('op-1')).toEqual(['operation', 'op-1']);
  });

  it('never retries client errors and backs off transient failures', () => {
    expect(shouldRetry(0, new ApiError(401, 'unauthorized'))).toBe(false);
    expect(shouldRetry(0, new ApiError(409, 'conflict'))).toBe(false);
    expect(shouldRetry(2, new Error('network'))).toBe(true);
    expect(shouldRetry(3, new Error('network'))).toBe(false);
    expect(retryDelay(0)).toBe(1000);
    expect(retryDelay(4)).toBe(8000);
  });

  it('stops an operation interval after a 404 even when deleting data remains cached', () => {
    const deleting: Operation = { operation_id: 'op-1', torrent_id: 'torrent-1', state: 'deleting' };
    expect(operationRefetchInterval(deleting, undefined)).toBe(1500);
    expect(operationRefetchInterval(deleting, new ApiError(404, 'unknown operation'))).toBe(false);
    expect(operationRefetchInterval({ ...deleting, state: 'deleted' }, undefined)).toBe(false);
  });

  it('stops the real operation hook after a 404 response', async () => {
    const missing = new ApiError(404, 'unknown operation');
    const api = {
      getOperation: vi.fn().mockResolvedValueOnce({ operation_id: 'op-1', torrent_id: 'torrent-1', state: 'deleting' }).mockRejectedValue(missing),
    } as unknown as ApiClient;
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    const wrapper = ({ children }: { children: React.ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
    const hook = renderHook(() => useOperation(api, 'op-1', true), { wrapper });

    await waitFor(() => expect(hook.result.current.data?.state).toBe('deleting'));
    let refetchError: unknown;
    await act(async () => {
      refetchError = (await hook.result.current.refetch()).error;
    });
    expect(refetchError).toBe(missing);
    await new Promise((resolve) => setTimeout(resolve, 1700));
    expect(api.getOperation).toHaveBeenCalledTimes(2);

    hook.unmount();
    queryClient.clear();
  });

  it('uploads torrent files sequentially and invalidates the list once', async () => {
    const firstTorrent = { id: 'torrent-1' };
    const secondTorrent = { id: 'torrent-2' };
    let resolveFirst!: (torrent: typeof firstTorrent) => void;
    const firstRequest = new Promise<typeof firstTorrent>((resolve) => { resolveFirst = resolve; });
    const api = {
      addTorrent: vi.fn().mockImplementationOnce(() => firstRequest).mockResolvedValueOnce(secondTorrent),
    } as unknown as ApiClient;
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    const invalidateQueries = vi.spyOn(queryClient, 'invalidateQueries');
    const wrapper = ({ children }: { children: React.ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
    const hook = renderHook(() => useAddTorrentFiles(api), { wrapper });
    const files = [new File(['one'], 'one.torrent'), new File(['two'], 'two.torrent')];
    const updates: string[] = [];

    const mutation = hook.result.current.mutateAsync({
      files,
      onFileStatus: ({ file, status }) => updates.push(`${file.name}:${status}`),
    });
    await waitFor(() => expect(api.addTorrent).toHaveBeenCalledTimes(1));
    resolveFirst(firstTorrent);
    const result = await act(async () => mutation);

    expect(api.addTorrent).toHaveBeenNthCalledWith(1, files[0]);
    expect(api.addTorrent).toHaveBeenNthCalledWith(2, files[1]);
    expect(result.torrents).toEqual([firstTorrent, secondTorrent]);
    expect(updates).toEqual([
      'one.torrent:uploading',
      'one.torrent:succeeded',
      'two.torrent:uploading',
      'two.torrent:succeeded',
    ]);
    expect(invalidateQueries).toHaveBeenCalledOnce();
  });

  it('continues file-level failures and stops on connection failures', async () => {
    const addTorrent = vi.fn()
      .mockResolvedValueOnce({ id: 'torrent-1' })
      .mockRejectedValueOnce(new ApiError(413, 'too large'))
      .mockResolvedValueOnce({ id: 'torrent-2' });
    const api = { addTorrent } as unknown as ApiClient;
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    const wrapper = ({ children }: { children: React.ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
    const hook = renderHook(() => useAddTorrentFiles(api), { wrapper });
    const files = [new File(['one'], 'one.torrent'), new File(['two'], 'two.torrent'), new File(['three'], 'three.torrent')];

    const result = await act(async () => hook.result.current.mutateAsync({ files }));
    expect(result.items.map((item) => item.status)).toEqual(['succeeded', 'failed', 'succeeded']);
    expect(addTorrent).toHaveBeenCalledTimes(3);

    addTorrent.mockReset().mockRejectedValueOnce(new TypeError('network down')).mockResolvedValueOnce({ id: 'later' });
    const stoppedFiles = [new File(['four'], 'four.torrent'), new File(['five'], 'five.torrent')];
    const stopped = await act(async () => hook.result.current.mutateAsync({ files: stoppedFiles }));
    expect(stopped.items.map((item) => item.status)).toEqual(['failed', 'skipped']);
    expect(addTorrent).toHaveBeenCalledOnce();
  });
});
