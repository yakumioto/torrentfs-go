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
});
