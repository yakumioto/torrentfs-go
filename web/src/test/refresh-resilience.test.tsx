import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { App } from '../app/App';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { queryKeys } from '../queries/keys';
import type { Torrent, TorrentStatus } from '../types/api';
import { theme } from '../styles/theme';

type FetchMock = ReturnType<typeof vi.fn>;

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

// A rate-limited poll is the shared policy's non-retryable background failure: `shouldRetry` never
// retries a client error, so the failed refresh settles in one attempt instead of racing the 5s poll.
function failingResponse(): Response {
  return jsonResponse({ error: 'too many requests' }, 429);
}

function makeTorrent(overrides: Partial<Torrent> = {}): Torrent {
  return {
    id: 'torrent-1',
    info_hash: 'abc123',
    name: 'Example torrent',
    state: 'ready',
    total_bytes: 100,
    cached_bytes: 25,
    created_at: '2026-09-17T00:00:00Z',
    ...overrides,
  };
}

function makeStatus(torrent: Torrent): TorrentStatus {
  return {
    torrent,
    metainfo_ready: true,
    piece_length: 16,
    pieces: [{ index: 0, cached: true, cached_bytes: 16, pinned: false }],
    files: [{ path: 'file.txt', size: 100, piece_start: 0, piece_end: 1 }],
  };
}

function renderApp(path: string, fetchMock: FetchMock) {
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
      <MantineProvider theme={theme} defaultColorScheme="dark">
        <AuthContext.Provider value={auth}>
          <MemoryRouter initialEntries={[path]}>
            <App />
          </MemoryRouter>
        </AuthContext.Provider>
      </MantineProvider>
    </QueryClientProvider>,
  );
  return { queryClient };
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('background refresh resilience', () => {
  it('keeps the last valid torrent list when a background refresh fails', async () => {
    const torrent = makeTorrent({ name: 'Ubuntu Desktop ISO' });
    let requests = 0;
    const fetchMock = vi.fn(() => {
      requests += 1;
      return Promise.resolve(requests === 1 ? jsonResponse([torrent]) : failingResponse());
    });

    const { queryClient } = renderApp('/', fetchMock);
    await waitFor(() => expect(screen.getByText('Ubuntu Desktop ISO')).toBeInTheDocument());
    expect(queryClient.getQueryData(queryKeys.torrents)).toHaveLength(1);

    await act(async () => {
      void queryClient.refetchQueries({ queryKey: queryKeys.torrents });
    });

    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Showing the last valid list'));
    expect(screen.getByText('Ubuntu Desktop ISO')).toBeInTheDocument();
    expect(screen.queryByText('Task list unavailable')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Loading torrents')).not.toBeInTheDocument();
    expect(queryClient.getQueryData(queryKeys.torrents)).toHaveLength(1);
  });

  it('keeps the last valid status snapshot when a background refresh fails', async () => {
    const torrent = makeTorrent({ name: 'Detail torrent', cached_bytes: 100 });
    let statusRequests = 0;
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith('/status')) {
        statusRequests += 1;
        return Promise.resolve(statusRequests === 1 ? jsonResponse(makeStatus(torrent)) : failingResponse());
      }
      return Promise.resolve(jsonResponse(torrent));
    });

    const { queryClient } = renderApp('/torrents/torrent-1', fetchMock);
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Detail torrent' })).toBeInTheDocument());
    expect(screen.getByRole('tab', { name: /Files/ })).toBeInTheDocument();

    await act(async () => {
      void queryClient.refetchQueries({ queryKey: queryKeys.torrentStatus('torrent-1') });
    });

    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Showing the last valid snapshot'));
    expect(screen.queryByText('Status snapshot unavailable')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('tab', { name: /Files/ }));
    expect(screen.getByText('file.txt')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('tab', { name: /Pieces/ }));
    expect(screen.getByRole('list', { name: 'Piece map' })).toBeInTheDocument();
  });
});
