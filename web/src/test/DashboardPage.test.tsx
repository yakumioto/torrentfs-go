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
import { ExperimentsPage } from '../pages/ExperimentsPage';
import { theme } from '../styles/theme';
import type { LabView, RuntimeStats, Torrent } from '../types/api';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

function torrent(overrides: Partial<Torrent> = {}): Torrent {
  return {
    id: 'torrent-1',
    info_hash: 'abc123',
    name: 'Alpha archive',
    category: '',
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

function runtimeStats(overrides: Partial<RuntimeStats> = {}): RuntimeStats {
  return {
    started_at: '2026-09-23T09:00:00Z',
    cache: { used_bytes: 1024, capacity_bytes: 4096 },
    transfer: { downloaded_bytes: 2048, uploaded_bytes: 1024 },
    ...overrides,
  };
}

function baselineLab(): LabView {
  return {
    settings: { mode: 'baseline', rate_bytes_per_second: 0, multiplier: 0, jitter_percent: 20, cap_bytes: 0 },
    seed: 0,
    announce_log: '',
    peer_sample_interval_seconds: 0,
    totals: {
      real_uploaded_bytes: 0,
      real_uploaded_at_last_announce_bytes: 0,
      reported_uploaded_bytes: 0,
      overreported_bytes: 0,
      pending_real_upload_bytes: 0,
    },
    torrents: [],
  };
}

function injectLab(overrides: Partial<LabView> = {}): LabView {
  return {
    settings: { mode: 'inject', rate_bytes_per_second: 2_097_152, multiplier: 0, jitter_percent: 20, cap_bytes: 0 },
    seed: 42,
    announce_log: '/var/log/torrentfs/lab.jsonl',
    peer_sample_interval_seconds: 5,
    totals: {
      real_uploaded_bytes: 5_000_000,
      real_uploaded_at_last_announce_bytes: 1000,
      reported_uploaded_bytes: 4_197_247,
      overreported_bytes: 4_196_247,
      pending_real_upload_bytes: 4_999_000,
    },
    torrents: [{
      info_hash: 'abc123',
      real_uploaded_bytes: 5_000_000,
      real_uploaded_at_last_announce_bytes: 1000,
      reported_uploaded_bytes: 4_197_247,
      last_announce_at: '2026-11-05T15:12:00.123456Z',
    }],
    ...overrides,
  };
}

function renderDashboard(
  torrents: Torrent[],
  statsBody: unknown = runtimeStats(),
  statsStatus = 200,
  listResponses: Torrent[][] = [torrents],
  labBody: unknown = baselineLab(),
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
    if (String(input).endsWith('/experiments/upload')) {
      return Promise.resolve(jsonResponse(labBody));
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
                <Route path="/experiments" element={<ExperimentsPage />} />
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

  it('filters to favorites only and restores the full list', async () => {
    renderDashboard([
      torrent({ id: 'starred', name: 'Alpha archive', favorite: true }),
      torrent({ id: 'plain', name: 'Beta movie', favorite: false }),
    ]);

    await waitFor(() => expect(screen.getByText('Beta movie')).toBeInTheDocument());

    const favoriteButton = screen.getByRole('button', { name: '收藏' });
    fireEvent.click(favoriteButton);
    expect(favoriteButton).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByText('Alpha archive')).toBeInTheDocument();
    expect(screen.queryByText('Beta movie')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: '全部' }));
    expect(screen.getByText('Beta movie')).toBeInTheDocument();
  });

  it('opens the prune dialog from the heading', async () => {
    renderDashboard([torrent()]);
    await waitFor(() => expect(screen.getByText('Alpha archive')).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: '批量清理' }));
    await waitFor(() => expect(screen.getByRole('dialog')).toHaveTextContent('已收藏的任务不会被删除'));
  });

  it('shows distinct empty states for no tasks and no matches', async () => {    renderDashboard([]);
    await waitFor(() => expect(screen.getByRole('heading', { name: '还没有任务' })).toBeInTheDocument());

    fireEvent.click(screen.getByRole('button', { name: '添加第一个任务' }));
    await waitFor(() => expect(screen.getByRole('tab', { name: '磁力链接' })).toBeInTheDocument());
  });

  it('shows the lab summary and per-torrent reported counters', async () => {
    renderDashboard([torrent({ uploaded_bytes: 1000 })], runtimeStats(), 200, undefined, injectLab());

    await waitFor(() => expect(screen.getByText('Alpha archive')).toBeInTheDocument());
    const card = screen.getByRole('link', { name: '上传实验：打开上传实验监控' });
    expect(card).toHaveTextContent('上传实验');
    expect(card).toHaveTextContent('注入虚报');
    expect(card).toHaveTextContent('实时真实上传');
    expect(card).toHaveTextContent('上次上报时真实');
    expect(card).toHaveTextContent('Tracker 上报');
    expect(card).toHaveTextContent('待上报真实增量');
    expect(card).toHaveTextContent('+4.0 MiB');
    expect(card).toHaveTextContent('上报值只在 tracker announce 时更新');

    const row = screen.getByRole('listitem');
    expect(row).toHaveTextContent('上报 4.0 MiB · 虚报 +4.0 MiB');
    // The row explains that the live-vs-reported gap is pending, not a bug.
    expect(screen.getByTitle('相对上次上报时真实量；待上报 4.8 MiB 尚未计入')).toBeInTheDocument();

    fireEvent.click(card);
    await waitFor(() => expect(screen.getByRole('heading', { name: '上传实验监控' })).toBeInTheDocument());
  });

  it('renders a dash for torrents without lab counters and shows the baseline card', async () => {
    renderDashboard([torrent({ info_hash: 'not-reported-yet' })], runtimeStats(), 200, undefined, injectLab({ torrents: [] }));

    await waitFor(() => expect(screen.getByText('Alpha archive')).toBeInTheDocument());
    expect(screen.getByText(/上报 — · 虚报 —/)).toBeInTheDocument();

    cleanup();
    renderDashboard([torrent()]);
    await waitFor(() => expect(screen.getByText('Alpha archive')).toBeInTheDocument());
    const baselineCard = screen.getByRole('link', { name: '上传实验：打开上传实验监控' });
    expect(baselineCard).toHaveTextContent('基线（真实上报）');
    expect(baselineCard).toHaveAttribute('href', '/experiments');
  });
});
