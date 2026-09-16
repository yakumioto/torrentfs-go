import { describe, expect, it } from 'vitest';
import { ApiError } from '../api/errors';
import { queryKeys } from '../queries/keys';
import { retryDelay, shouldRetry } from '../queries/retry';

describe('query contracts', () => {
  it('keeps the planned cache key shapes', () => {
    expect(queryKeys.torrents).toEqual(['torrents']);
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
});
