import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from '../api/client';
import { ApiError } from '../api/errors';
import { operationRefetchInterval, useAddTorrentFiles, useLab, useOperation, useSetLabSettings } from '../queries/hooks';
import { queryKeys } from '../queries/keys';
import { retryDelay, shouldRetry } from '../queries/retry';
import type { LabView, Operation } from '../types/api';

function labView(overrides: Partial<LabView> = {}): LabView {
  return {
    enabled: true,
    settings: { mode: 'inject', rate_bytes_per_second: 2_097_152, multiplier: 0, jitter_percent: 20, cap_bytes: 0 },
    seed: 42,
    announce_log: '/var/log/torrentfs/lab.jsonl',
    peer_sample_interval_seconds: 5,
    totals: {
      real_uploaded_bytes: 5_000_000,
      real_uploaded_at_last_announce_bytes: 0,
      reported_uploaded_bytes: 4_197_247,
      overreported_bytes: 4_197_247,
      pending_real_upload_bytes: 5_000_000,
    },
    torrents: [{
      info_hash: 'abc123',
      real_uploaded_bytes: 5_000_000,
      real_uploaded_at_last_announce_bytes: 0,
      reported_uploaded_bytes: 4_197_247,
      last_announce_at: '2026-11-05T15:12:00.123456Z',
    }],
    ...overrides,
  };
}

describe('query contracts', () => {
  it('keeps the planned cache key shapes', () => {
    expect(queryKeys.torrents).toEqual(['torrents']);
    expect(queryKeys.categories).toEqual(['categories']);
    expect(queryKeys.runtimeStats).toEqual(['runtime-stats']);
    expect(queryKeys.lab).toEqual(['lab']);
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

  it('stops the real operation hook after a 404 response', async () => {    const missing = new ApiError(404, 'unknown operation');
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

  it('polls the live lab view only while enabled', async () => {
    const api = { getLab: vi.fn().mockResolvedValue(labView()) } as unknown as ApiClient;
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    const wrapper = ({ children }: { children: React.ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;

    const disabled = renderHook(() => useLab(api, false), { wrapper });
    expect(disabled.result.current.fetchStatus).toBe('idle');
    expect(api.getLab).not.toHaveBeenCalled();
    disabled.unmount();

    const enabled = renderHook(() => useLab(api, true), { wrapper });
    await waitFor(() => expect(enabled.result.current.data?.totals.reported_uploaded_bytes).toBe(4_197_247));
    expect(api.getLab).toHaveBeenCalledOnce();

    enabled.unmount();
    queryClient.clear();
  });

  it('stores the applied lab view returned by the settings write', async () => {
    const applied = labView({
      settings: { mode: 'amplify', rate_bytes_per_second: 0, multiplier: 3, jitter_percent: 20, cap_bytes: 0 },
    });
    const api = { setLabSettings: vi.fn().mockResolvedValue(applied) } as unknown as ApiClient;
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } } });
    const wrapper = ({ children }: { children: React.ReactNode }) => <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
    const hook = renderHook(() => useSetLabSettings(api), { wrapper });

    await act(async () => {
      await hook.result.current.mutateAsync({ settings: { mode: 'amplify', multiplier: 3 } });
    });

    expect(api.setLabSettings).toHaveBeenCalledWith({ mode: 'amplify', multiplier: 3 }, undefined);
    expect(queryClient.getQueryData(queryKeys.lab)).toEqual(applied);

    hook.unmount();
    queryClient.clear();
  });
});
