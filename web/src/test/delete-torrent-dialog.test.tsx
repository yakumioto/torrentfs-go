import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { DeleteTorrentDialog } from '../components/dialogs/DeleteTorrentDialog';
import { theme } from '../styles/theme';
import type { Operation, Torrent } from '../types/api';

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
}

function makeTorrent(): Torrent {
  return {
    id: 'torrent-1',
    info_hash: 'abc123',
    name: 'Example torrent',
    state: 'ready',
    total_bytes: 100,
    downloaded_bytes: 0,
    uploaded_bytes: 0,
    cached_bytes: 25,
    created_at: '2026-09-17T00:00:00Z',
    favorite: false,
  };
}

function renderDialog(fetchMock: ReturnType<typeof vi.fn>) {
  const api = new ApiClient();
  const auth: AuthContextValue = {
    api,
    phase: 'anonymous',
    isReady: true,
    isAuthenticated: false,
    loginError: '',
    sessionNotice: '',
    connectionError: '',
    login: async () => false,
    logout: async () => undefined,
    retryProbe: () => undefined,
  };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });

  vi.stubGlobal('fetch', fetchMock);
  render(
    <QueryClientProvider client={queryClient}>
      <MantineProvider theme={theme} defaultColorScheme="light">
        <AuthContext.Provider value={auth}>
          <DeleteTorrentDialog torrent={makeTorrent()} opened onClose={() => undefined} />
        </AuthContext.Provider>
      </MantineProvider>
    </QueryClientProvider>,
  );
}

function deletionUrl(fetchMock: ReturnType<typeof vi.fn>): string {
  const call = fetchMock.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === 'DELETE');
  return call === undefined ? '' : String(call[0]);
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('DeleteTorrentDialog', () => {
  it('submits a plain delete with no purge parameter or purge control', async () => {
    const operation: Operation = { operation_id: 'op-1', torrent_id: 'torrent-1', state: 'deleting' };
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(operation)));
    renderDialog(fetchMock);

    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument();

    const submit = screen.getByRole('button', { name: '删除任务' });
    expect(submit).toBeEnabled();
    fireEvent.click(submit);

    await waitFor(() => expect(deletionUrl(fetchMock)).not.toBe(''));
    expect(deletionUrl(fetchMock)).toMatch(/\/api\/v1\/torrents\/torrent-1$/);
    expect(deletionUrl(fetchMock)).not.toContain('purge_data');
  });

  it('reports the deletion operation after the request succeeds', async () => {
    const operation: Operation = { operation_id: 'op-1', torrent_id: 'torrent-1', state: 'deleting' };
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(operation)));
    renderDialog(fetchMock);

    fireEvent.click(screen.getByRole('button', { name: '删除任务' }));

    await waitFor(() => expect(screen.getByText('正在删除任务…')).toBeInTheDocument());
  });

  it('warns that managed subtitles are removed with the task', () => {
    renderDialog(vi.fn(() => Promise.resolve(jsonResponse({ operation_id: 'op-1', torrent_id: 'torrent-1', state: 'deleting' }))));
    expect(screen.getByText(/由 TorrentFS 管理的字幕文件会一并清理/)).toBeInTheDocument();
  });

  it('explains a subtitle cleanup failure and how to retry', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      if (String(input).endsWith('/operations/op-1')) {
        return Promise.resolve(jsonResponse({
          operation_id: 'op-1',
          torrent_id: 'torrent-1',
          state: 'delete_failed',
          error: 'subtitle cleanup failed',
          error_code: 'subtitle_cleanup_failed',
        }));
      }
      return Promise.resolve(jsonResponse({ operation_id: 'op-1', torrent_id: 'torrent-1', state: 'deleting' }));
    });
    renderDialog(fetchMock);

    fireEvent.click(screen.getByRole('button', { name: '删除任务' }));

    await waitFor(() => expect(screen.getByText('字幕文件清理失败')).toBeInTheDocument());
    expect(screen.getByText(/再次删除同一任务即可重试/)).toBeInTheDocument();
    // The retry stays available rather than reporting success.
    expect(screen.getByRole('button', { name: '删除任务' })).toBeEnabled();
    expect(screen.queryByText('任务已删除')).not.toBeInTheDocument();
  });
});
