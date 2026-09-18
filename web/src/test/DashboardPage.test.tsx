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
import type { Torrent } from '../types/api';

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
    cached_bytes: 25,
    created_at: '2026-09-17T00:00:00Z',
    ...overrides,
  };
}

function renderDashboard(torrents: Torrent[]) {
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
  const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(torrents)));
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

describe('DashboardPage', () => {
  it('filters by search and status while keeping summary data visible', async () => {
    renderDashboard([
      torrent(),
      torrent({ id: 'torrent-2', name: 'Beta movie', state: 'adding' }),
      torrent({ id: 'torrent-3', name: 'Broken archive', state: 'error' }),
    ]);

    await waitFor(() => expect(screen.getByText('Alpha archive')).toBeInTheDocument());
    expect(screen.getByRole('heading', { name: '任务列表' })).toBeInTheDocument();
    expect(screen.getByText('全部任务')).toBeInTheDocument();
    expect(screen.getByText('全部任务').parentElement).toHaveTextContent('3');

    fireEvent.change(screen.getByRole('textbox', { name: '搜索任务' }), { target: { value: 'Beta' } });
    expect(screen.getByText('Beta movie')).toBeInTheDocument();
    expect(screen.queryByText('Alpha archive')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: '清除搜索' }));
    fireEvent.click(screen.getByRole('button', { name: '错误' }));
    expect(screen.getByText('Broken archive')).toBeInTheDocument();
    expect(screen.queryByText('Beta movie')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: '全部' }));
    fireEvent.change(screen.getByRole('textbox', { name: '搜索任务' }), { target: { value: 'does-not-exist' } });
    expect(screen.getByRole('heading', { name: '没有匹配的任务' })).toBeInTheDocument();
  });

  it('shows distinct empty states for no tasks and no matches', async () => {
    renderDashboard([]);
    await waitFor(() => expect(screen.getByRole('heading', { name: '还没有任务' })).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: '添加第一个任务' }));
    await waitFor(() => expect(screen.getByRole('tab', { name: '磁力链接' })).toBeInTheDocument());
  });
});
