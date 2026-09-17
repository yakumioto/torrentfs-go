import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { TorrentDetailPage } from '../pages/TorrentDetailPage';
import { queryKeys } from '../queries/keys';
import type { Torrent, TorrentStatus } from '../types/api';
import { theme } from '../styles/theme';

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
}

function makeTorrent(overrides: Partial<Torrent> = {}): Torrent {
  return {
    id: 'torrent-1',
    info_hash: 'abc123',
    name: 'Stale detail title',
    state: 'adding',
    total_bytes: 100,
    completed_bytes: 0,
    progress: 0,
    created_at: '2026-09-17T00:00:00Z',
    ...overrides,
  };
}

function makeStatus(torrent: Torrent): TorrentStatus {
  return { torrent, metainfo_ready: true, piece_length: 16, pieces: [], files: [] };
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('TorrentDetailPage', () => {
  it('uses the latest status torrent for metadata and pending state', async () => {
    const detailTorrent = makeTorrent();
    const statusSnapshots = [
      makeStatus(makeTorrent({ name: 'Resolved detail title', state: 'downloading', progress: 0.35, completed_bytes: 35 })),
      makeStatus(makeTorrent({ name: 'Updated detail title', state: 'seeding', progress: 1, completed_bytes: 100 })),
    ];
    let statusIndex = 0;
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith('/status')) {
        const snapshot = statusSnapshots[Math.min(statusIndex++, statusSnapshots.length - 1)];
        return Promise.resolve(jsonResponse(snapshot));
      }
      return Promise.resolve(jsonResponse(detailTorrent));
    });
    vi.stubGlobal('fetch', fetchMock);

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

    render(
      <QueryClientProvider client={queryClient}>
        <MantineProvider theme={theme} defaultColorScheme="dark">
          <AuthContext.Provider value={auth}>
            <MemoryRouter initialEntries={['/torrents/torrent-1']}>
              <Routes><Route path="/torrents/:id" element={<TorrentDetailPage />} /></Routes>
            </MemoryRouter>
          </AuthContext.Provider>
        </MantineProvider>
      </QueryClientProvider>,
    );

    await waitFor(() => expect(screen.getByRole('heading', { name: 'Resolved detail title' })).toBeInTheDocument());
    expect(screen.queryByText('Waiting for metadata')).not.toBeInTheDocument();
    expect(screen.getAllByText('Downloading')).toHaveLength(2);

    await queryClient.refetchQueries({ queryKey: queryKeys.torrentStatus('torrent-1') });
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Updated detail title' })).toBeInTheDocument());
    expect(screen.getAllByText('Seeding')).toHaveLength(2);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });
});
