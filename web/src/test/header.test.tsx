import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { App } from '../app/App';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import type { Torrent, TorrentStatus } from '../types/api';
import { theme } from '../styles/theme';

type FetchMock = ReturnType<typeof vi.fn>;

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
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

  window.history.replaceState({}, '', path);
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
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  window.history.replaceState({}, '', '/');
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('Header refresh control', () => {
  it('refreshes only the torrent list without navigation on the dashboard', async () => {
    const torrent = makeTorrent();
    let requestCount = 0;
    let resolveRefresh: ((response: Response) => void) | undefined;
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      void input;
      void init;
      requestCount += 1;
      if (requestCount === 1) {
        return Promise.resolve(jsonResponse([torrent]));
      }
      return new Promise<Response>((resolve) => {
        resolveRefresh = resolve;
      });
    });
    const navigationError = vi.spyOn(console, 'error').mockImplementation(() => undefined);

    renderApp('/', fetchMock);
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    fetchMock.mockClear();

    const refreshButton = screen.getByRole('button', { name: 'Refresh data' });
    expect(refreshButton).toHaveAttribute('type', 'button');
    fireEvent.click(refreshButton);

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(String(fetchMock.mock.calls[0]?.[0])).toMatch(/\/api\/v1\/torrents$/);
    expect(fetchMock.mock.calls[0]?.[1]?.method ?? 'GET').toBe('GET');
    expect(fetchMock.mock.calls.some(([input]) => String(input).includes('/status'))).toBe(false);
    expect(screen.queryByText(/Updating the latest snapshot/)).not.toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Torrents' })).toBeInTheDocument();
    expect(screen.getByText(torrent.name)).toBeInTheDocument();
    expect(window.location.pathname).toBe('/');
    expect(document.querySelector('form')).toBeNull();

    expect(resolveRefresh).toBeDefined();
    resolveRefresh?.(jsonResponse([torrent]));
    expect(screen.queryByText(/Updating the latest snapshot/)).not.toBeInTheDocument();
    expect(navigationError.mock.calls.flat().some((value) => String(value).includes('Not implemented: navigation'))).toBe(false);
  });

  it('refreshes only the active status query on detail pages', async () => {
    const torrent = makeTorrent({ name: 'Detail torrent', cached_bytes: 100 });
    const status = makeStatus(torrent);
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      void init;
      const url = String(input);
      if (url.endsWith('/status')) {
        return Promise.resolve(jsonResponse(status));
      }
      if (url.endsWith('/torrent-1')) {
        return Promise.resolve(jsonResponse(torrent));
      }
      throw new Error(`Unexpected request: ${url}`);
    });

    renderApp('/torrents/torrent-1', fetchMock);
    await waitFor(() => expect(screen.getByRole('heading', { name: torrent.name })).toBeInTheDocument());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(fetchMock.mock.calls.some(([input]) => String(input).endsWith('/api/v1/torrents'))).toBe(false);
    fetchMock.mockClear();

    fireEvent.click(screen.getByRole('button', { name: 'Refresh data' }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));

    expect(String(fetchMock.mock.calls[0]?.[0])).toMatch(/\/api\/v1\/torrents\/torrent-1\/status$/);
    expect(fetchMock.mock.calls[0]?.[1]?.method ?? 'GET').toBe('GET');
    expect(fetchMock.mock.calls.some(([input]) => String(input).endsWith('/api/v1/torrents'))).toBe(false);
    expect(screen.getByRole('heading', { name: torrent.name })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: /Files/ })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: /Pieces/ })).toBeInTheDocument();
    expect(window.location.pathname).toBe('/torrents/torrent-1');
  });
});
