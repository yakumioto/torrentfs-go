import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ApiClient } from '../api/client';
import { ApiError } from '../api/errors';
import { operationRefetchInterval, useOperation } from '../queries/hooks';
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
});
