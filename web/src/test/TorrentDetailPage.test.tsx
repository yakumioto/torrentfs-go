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
    cached_bytes: 0,
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
      makeStatus(makeTorrent({ name: 'Resolved detail title', state: 'ready', cached_bytes: 35 })),
      makeStatus(makeTorrent({ name: 'Updated detail title', state: 'error', cached_bytes: 100 })),
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
        <MantineProvider theme={theme} defaultColorScheme="light">
          <AuthContext.Provider value={auth}>
            <MemoryRouter initialEntries={['/torrents/torrent-1']}>
              <Routes><Route path="/torrents/:id" element={<TorrentDetailPage />} /></Routes>
            </MemoryRouter>
          </AuthContext.Provider>
        </MantineProvider>
      </QueryClientProvider>,
    );

    await waitFor(() => expect(screen.getByRole('heading', { name: 'Resolved detail title' })).toBeInTheDocument());
    expect(screen.queryByText('正在等待元数据')).not.toBeInTheDocument();
    expect(screen.getAllByText('就绪')).toHaveLength(2);

    await queryClient.refetchQueries({ queryKey: queryKeys.torrentStatus('torrent-1') });
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Updated detail title' })).toBeInTheDocument());
    expect(screen.getAllByText('错误')).toHaveLength(2);
    expect(fetchMock).toHaveBeenCalledTimes(3);
  });

  it('keeps Torrent, file, and absolute piece scopes explicit', async () => {
    const pieceLength = 4 * 1024 * 1024;
    const totalBytes = 8131 * pieceLength;
    const fileBytes = 344 * pieceLength;
    const torrent = makeTorrent({
      name: 'Geometric fixture',
      state: 'ready',
      total_bytes: totalBytes,
      cached_bytes: Math.floor(1.1 * 1024 * 1024 * 1024),
    });
    const status: TorrentStatus = {
      torrent,
      metainfo_ready: true,
      piece_length: pieceLength,
      pieces: Array.from({ length: 8131 }, (_, index) => ({ index, cached: index < 344, cached_bytes: index < 344 ? pieceLength : 0, pinned: false })),
      files: [{ path: 'video.mkv', size: fileBytes, piece_start: 0, piece_end: 344 }],
    };
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      if (String(input).endsWith('/status')) {
        return Promise.resolve(jsonResponse(status));
      }
      return Promise.resolve(jsonResponse(torrent));
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
        <MantineProvider theme={theme} defaultColorScheme="light">
          <AuthContext.Provider value={auth}>
            <MemoryRouter initialEntries={['/torrents/torrent-1']}>
              <Routes><Route path="/torrents/:id" element={<TorrentDetailPage />} /></Routes>
            </MemoryRouter>
          </AuthContext.Provider>
        </MantineProvider>
      </QueryClientProvider>,
    );

    await waitFor(() => expect(screen.getByRole('heading', { name: 'Geometric fixture' })).toBeInTheDocument());
    expect(screen.getByText('Torrent 总大小')).toBeInTheDocument();
    expect(screen.getAllByText('Torrent 缓存占用').length).toBeGreaterThan(0);
    expect(screen.getAllByText('1.3 GiB').length).toBeGreaterThan(0);
    expect(screen.getAllByText('32 GiB').length).toBeGreaterThan(0);
    expect(screen.getByText('文件涉及的数据块范围（Torrent 绝对 piece 索引）')).toBeInTheDocument();
    expect(screen.getByText('所涉 piece 当前驻留内存缓存')).toBeInTheDocument();
    expect(screen.getByText(/不是文件下载百分比或播放进度/)).toBeInTheDocument();
  });
});
