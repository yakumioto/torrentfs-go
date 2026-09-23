import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { queryKeys } from '../queries/keys';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { AppLayout } from '../components/layout/AppLayout';
import { DashboardPage } from '../pages/DashboardPage';
import { theme } from '../styles/theme';
import type { RuntimeStats, Torrent } from '../types/api';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
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
    ...overrides,
  };
}

function runtimeStats(overrides: Partial<RuntimeStats> = {}): RuntimeStats {
  return {
    started_at: '2026-09-23T09:00:00Z',
    cache: { used_bytes: 1024, capacity_bytes: 4096 },
    transfer: { downloaded_bytes: 2048, uploaded_bytes: 1024 },
    ...overrides,
  };
}

function renderDashboard(
  torrents: Torrent[],
  statsBody: unknown = runtimeStats(),
  statsStatus = 200,
  listResponses: Torrent[][] = [torrents],
) {
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
  let listRequest = 0;
  const fetchMock = vi.fn((input: RequestInfo | URL) => {
    if (String(input).endsWith('/stats')) {
      return Promise.resolve(jsonResponse(statsBody, statsStatus));
    }
    const response = listResponses[Math.min(listRequest++, listResponses.length - 1)] ?? [];
    return Promise.resolve(jsonResponse(response));
  });
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
  return { queryClient };
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

  it('shows runtime stats, removes the list cache column, and sorts the visible fields', async () => {
    renderDashboard([
      torrent({ id: 'older', name: 'Zulu', created_at: '2026-09-16T00:00:00Z', downloaded_bytes: 2048, uploaded_bytes: 512 }),
      torrent({ id: 'newer', name: 'Alpha', created_at: '2026-09-17T00:00:00Z', downloaded_bytes: 4096, uploaded_bytes: 1536 }),
    ]);

    await waitFor(() => expect(screen.getByText('Alpha')).toBeInTheDocument());
    expect(screen.getByText('全局缓存')).toBeInTheDocument();
    expect(screen.getByText('本次启动下载')).toBeInTheDocument();
    expect(screen.getByText('本次启动上传')).toBeInTheDocument();
    expect(screen.queryByText('缓存占用')).not.toBeInTheDocument();
    expect(screen.getByRole('columnheader', { name: '下载量' })).toBeInTheDocument();
    expect(screen.getByRole('columnheader', { name: '上传量' })).toBeInTheDocument();
    const initialRows = screen.getAllByRole('listitem');
    expect(initialRows[0]).toHaveTextContent('4.0 KiB');
    expect(initialRows[0]).toHaveTextContent('1.5 KiB');
    expect(screen.queryByRole('button', { name: '按下载量排序' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: '按上传量排序' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: '按大小排序' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '按状态排序' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '按添加时间排序，当前降序' })).toBeInTheDocument();

    const rows = () => screen.getAllByRole('listitem').map((item) => item.textContent);
    expect(rows()[0]).toContain('Alpha');
    fireEvent.click(screen.getByRole('button', { name: '按任务排序' }));
    expect(rows()[0]).toContain('Alpha');
    expect(rows()[1]).toContain('Zulu');
    expect(screen.getByLabelText('移动端排序字段')).toHaveValue('name');

    fireEvent.click(screen.getByRole('button', { name: '按任务排序，当前升序' }));
    expect(rows()[0]).toContain('Zulu');
    expect(rows()[1]).toContain('Alpha');
    expect(screen.getByRole('button', { name: '按任务排序，当前降序' })).toBeInTheDocument();
  });

  it('updates per-torrent transfer values after a successful poll without changing sort', async () => {
    const initial = [
      torrent({ id: 'older', name: 'Zulu', created_at: '2026-09-16T00:00:00Z', downloaded_bytes: 512, uploaded_bytes: 256 }),
      torrent({ id: 'newer', name: 'Alpha', created_at: '2026-09-17T00:00:00Z', downloaded_bytes: 1024, uploaded_bytes: 512 }),
    ];
    const refreshed = [
      torrent({ id: 'older', name: 'Zulu', created_at: '2026-09-16T00:00:00Z', downloaded_bytes: 1024, uploaded_bytes: 512 }),
      torrent({ id: 'newer', name: 'Alpha', created_at: '2026-09-17T00:00:00Z', downloaded_bytes: 3072, uploaded_bytes: 2048 }),
    ];
    const { queryClient } = renderDashboard(initial, runtimeStats(), 200, [initial, refreshed]);

    await waitFor(() => expect(screen.getByText('Alpha')).toBeInTheDocument());
    const rows = () => screen.getAllByRole('listitem');
    expect(rows()[0]).toHaveTextContent('Alpha');
    expect(rows()[0]).toHaveTextContent('1.0 KiB');

    await act(async () => {
      await queryClient.refetchQueries({ queryKey: queryKeys.torrents });
    });

    await waitFor(() => expect(rows()[0]).toHaveTextContent('3.0 KiB'));
    expect(rows()[0]).toHaveTextContent('2.0 KiB');
    expect(rows()[0]).toHaveTextContent('Alpha');
    expect(rows()[1]).toHaveTextContent('Zulu');
  });

  it('keeps the task list usable when runtime stats are unavailable', async () => {
    renderDashboard([torrent()], { error: 'not found' }, 404);

    await waitFor(() => expect(screen.getByText('Alpha archive')).toBeInTheDocument());
    expect(screen.getByRole('status', { name: '' })).toHaveTextContent('运行时统计暂不可用');
    expect(screen.queryByText('任务列表暂不可用')).not.toBeInTheDocument();
  });

  it('shows distinct empty states for no tasks and no matches', async () => {
    renderDashboard([]);
    await waitFor(() => expect(screen.getByRole('heading', { name: '还没有任务' })).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: '添加第一个任务' }));
    await waitFor(() => expect(screen.getByRole('tab', { name: '磁力链接' })).toBeInTheDocument());
  });
});
