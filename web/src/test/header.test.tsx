import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { Header } from '../components/layout/Header';
import { useTorrentList, useTorrentStatus } from '../queries/hooks';
import { theme } from '../styles/theme';

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
}

function QueryHarness({ api, statusId }: { api: ApiClient; statusId: string }) {
  useTorrentList(api, true);
  useTorrentStatus(api, statusId, statusId !== '');
  return <div data-testid="route-sentinel">Current route remains mounted</div>;
}

function renderHeader(path: string, fetchMock: ReturnType<typeof vi.fn>) {
  const api = new ApiClient();
  const auth: AuthContextValue = {
    api,
    phase: 'anonymous',
    isReady: true,
    isAuthenticated: false,
    loginError: '',
    connectionError: '',
    login: async () => false,
    logout: async () => undefined,
    retryProbe: () => undefined,
  };
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const statusId = path.startsWith('/torrents/') ? path.split('/').pop() ?? '' : '';

  vi.stubGlobal('fetch', fetchMock);
  render(
    <QueryClientProvider client={queryClient}>
      <MantineProvider theme={theme} defaultColorScheme="dark">
        <AuthContext.Provider value={auth}>
          <MemoryRouter initialEntries={[path]}>
            <Routes>
              <Route
                path="*"
                element={
                  <>
                    <Header onAdd={vi.fn()} />
                    <QueryHarness api={api} statusId={statusId} />
                  </>
                }
              />
            </Routes>
          </MemoryRouter>
        </AuthContext.Provider>
      </MantineProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('Header refresh control', () => {
  it('refreshes only the torrent list without navigation on the dashboard', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      void init;
      return Promise.resolve(jsonResponse(String(input).endsWith('/status') ? {} : []));
    });
    const navigationError = vi.spyOn(console, 'error').mockImplementation(() => undefined);

    renderHeader('/', fetchMock);
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    fetchMock.mockClear();

    const refreshButton = screen.getByRole('button', { name: 'Refresh data' });
    expect(refreshButton).toHaveAttribute('type', 'button');
    fireEvent.click(refreshButton);

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(String(fetchMock.mock.calls[0]?.[0])).toMatch(/\/api\/v1\/torrents$/);
    expect(fetchMock.mock.calls[0]?.[1]?.method ?? 'GET').toBe('GET');
    expect(fetchMock.mock.calls.some(([input]) => String(input).includes('/status'))).toBe(false);
    expect(screen.getByTestId('route-sentinel')).toBeInTheDocument();
    expect(window.location.pathname).toBe('/');
    expect(document.querySelector('form')).toBeNull();
    expect(navigationError.mock.calls.flat().some((value) => String(value).includes('Not implemented: navigation'))).toBe(false);
  });

  it('refreshes the torrent list and current torrent status on detail pages', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      return Promise.resolve(jsonResponse(url.endsWith('/status') ? {} : []));
    });

    renderHeader('/torrents/torrent-1', fetchMock);
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    fetchMock.mockClear();

    fireEvent.click(screen.getByRole('button', { name: 'Refresh data' }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));

    const urls = fetchMock.mock.calls.map(([input]) => String(input));
    expect(urls).toEqual(expect.arrayContaining([
      expect.stringMatching(/\/api\/v1\/torrents$/),
      expect.stringMatching(/\/api\/v1\/torrents\/torrent-1\/status$/),
    ]));
  });
});
