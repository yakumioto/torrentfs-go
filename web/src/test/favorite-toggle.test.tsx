import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { AppLayout } from '../components/layout/AppLayout';
import { DashboardPage } from '../pages/DashboardPage';
import { theme } from '../styles/theme';
import type { RuntimeStats, Torrent } from '../types/api';

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
}

function torrent(overrides: Partial<Torrent> = {}): Torrent {
  return {
    id: 'torrent-1',
    info_hash: 'abc123',
    name: 'Alpha archive',
    state: 'ready',
    total_bytes: 100,
    downloaded_bytes: 0,
    uploaded_bytes: 0,
    cached_bytes: 25,
    created_at: '2026-09-17T00:00:00Z',
    favorite: false,
    ...overrides,
  };
}

function runtimeStats(): RuntimeStats {
  return {
    started_at: '2026-09-23T09:00:00Z',
    cache: { used_bytes: 1024, capacity_bytes: 4096 },
    transfer: { downloaded_bytes: 2048, uploaded_bytes: 1024 },
  };
}

function renderDashboard(fetchMock: ReturnType<typeof vi.fn>) {
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
    <MantineProvider theme={theme} defaultColorScheme="light">
      <QueryClientProvider client={queryClient}>
        <AuthContext.Provider value={auth}>
          <MemoryRouter initialEntries={['/']}>
            <Routes>
              <Route element={<AppLayout />}>
                <Route path="/" element={<DashboardPage />} />
              </Route>
            </Routes>
          </MemoryRouter>
        </AuthContext.Provider>
      </QueryClientProvider>
    </MantineProvider>,
  );
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('favorite toggle', () => {
  it('sends a favorite update for the clicked row only', async () => {
    let favorited = false;
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.endsWith('/stats')) {
        return Promise.resolve(jsonResponse(runtimeStats()));
      }
      if (init?.method === 'PUT') {
        favorited = true;
        return Promise.resolve(jsonResponse(torrent({ favorite: true })));
      }
      return Promise.resolve(jsonResponse([
        torrent({ favorite: favorited }),
        torrent({ id: 'torrent-2', name: 'Beta movie', favorite: false }),
      ]));
    });
    renderDashboard(fetchMock);

    await waitFor(() => expect(screen.getByText('Alpha archive')).toBeInTheDocument());

    const star = screen.getByRole('button', { name: '收藏 Alpha archive' });
    expect(star).toHaveAttribute('aria-pressed', 'false');
    fireEvent.click(star);

    await waitFor(() => expect(screen.getByRole('button', { name: '取消收藏 Alpha archive' })).toHaveAttribute('aria-pressed', 'true'));
    expect(screen.getByRole('button', { name: '收藏 Beta movie' })).toHaveAttribute('aria-pressed', 'false');

    const put = fetchMock.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === 'PUT');
    expect(put).toBeDefined();
    expect(String(put?.[0])).toBe('/api/v1/torrents/torrent-1/favorite');
    expect((put?.[1] as RequestInit).body).toBe(JSON.stringify({ favorite: true }));
  });
});
