import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { HandoffTorrentDialog } from '../components/dialogs/HandoffTorrentDialog';
import { theme } from '../styles/theme';
import type { HandoffTarget, Operation, Torrent } from '../types/api';

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
}

function errorResponse(status: number, code: string, message: string): Response {
  return new Response(JSON.stringify({ error: message, code }), { status, headers: { 'Content-Type': 'application/json' } });
}

function makeTorrent(): Torrent {
  return {
    id: 'torrent-1',
    info_hash: 'abc123',
    name: 'Example torrent',
    category: '',
    state: 'ready',
    total_bytes: 100,
    downloaded_bytes: 0,
    uploaded_bytes: 0,
    cached_bytes: 25,
    created_at: '2026-09-17T00:00:00Z',
    favorite: false,
  };
}

function renderDialog(fetchMock: ReturnType<typeof vi.fn>, target: HandoffTarget = { configured: true, target: 'http://nas:9091' }) {
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
          <HandoffTorrentDialog torrent={makeTorrent()} target={target} opened onClose={() => undefined} />
        </AuthContext.Provider>
      </MantineProvider>
    </QueryClientProvider>,
  );
}

function handoffUrl(fetchMock: ReturnType<typeof vi.fn>): string {
  const call = fetchMock.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === 'POST');
  return call === undefined ? '' : String(call[0]);
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('HandoffTorrentDialog', () => {
  it('explains that the local task is deleted after a successful handoff', () => {
    renderDialog(vi.fn(() => Promise.resolve(jsonResponse({ operation_id: 'op-1', state: 'running' }))));
    expect(screen.getByText(/成功后从本机删除/)).toBeInTheDocument();
    expect(screen.getByText(/移交失败会自动复原/)).toBeInTheDocument();
  });

  it('posts the handoff and tracks the operation', async () => {
    const operation: Operation = { operation_id: 'op-1', torrent_id: 'torrent-1', state: 'running' };
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(operation)));
    renderDialog(fetchMock);

    fireEvent.click(screen.getByRole('button', { name: '移交并删除' }));

    await waitFor(() => expect(handoffUrl(fetchMock)).toMatch(/\/api\/v1\/torrents\/torrent-1\/handoff$/));
    await waitFor(() => expect(screen.getByText('正在移交任务…')).toBeInTheDocument());
  });

  it('disables the submit control once the handoff is running', async () => {
    const operation: Operation = { operation_id: 'op-1', torrent_id: 'torrent-1', state: 'running' };
    renderDialog(vi.fn(() => Promise.resolve(jsonResponse(operation))));

    fireEvent.click(screen.getByRole('button', { name: '移交并删除' }));

    await waitFor(() => expect(screen.getByRole('button', { name: '移交并删除' })).toBeDisabled());
  });

  it('reports a successful handoff', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      if (String(input).endsWith('/operations/op-1')) {
        return Promise.resolve(jsonResponse({ operation_id: 'op-1', torrent_id: 'torrent-1', state: 'completed' }));
      }
      return Promise.resolve(jsonResponse({ operation_id: 'op-1', torrent_id: 'torrent-1', state: 'running' }));
    });
    renderDialog(fetchMock);

    fireEvent.click(screen.getByRole('button', { name: '移交并删除' }));

    await waitFor(() => expect(screen.getByText('任务已移交')).toBeInTheDocument());
  });

  it('explains a restored task after a failed delivery', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      if (String(input).endsWith('/operations/op-1')) {
        return Promise.resolve(jsonResponse({
          operation_id: 'op-1',
          torrent_id: 'torrent-1',
          state: 'restored',
          error: 'transmission unreachable',
          error_code: 'handoff_unreachable',
        }));
      }
      return Promise.resolve(jsonResponse({ operation_id: 'op-1', torrent_id: 'torrent-1', state: 'running' }));
    });
    renderDialog(fetchMock);

    fireEvent.click(screen.getByRole('button', { name: '移交并删除' }));

    await waitFor(() => expect(screen.getByText('移交失败，任务已复原')).toBeInTheDocument());
    expect(screen.getByText(/无法连接 Transmission/)).toBeInTheDocument();
  });

  it('reports a local deletion failure after the target accepted the torrent', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      if (String(input).endsWith('/operations/op-1')) {
        return Promise.resolve(jsonResponse({
          operation_id: 'op-1',
          torrent_id: 'torrent-1',
          state: 'failed',
          error: 'remove metainfo: permission denied',
        }));
      }
      return Promise.resolve(jsonResponse({ operation_id: 'op-1', torrent_id: 'torrent-1', state: 'running' }));
    });
    renderDialog(fetchMock);

    fireEvent.click(screen.getByRole('button', { name: '移交并删除' }));

    await waitFor(() => expect(screen.getByText('已移交，但本机删除失败')).toBeInTheDocument());
    expect(screen.getByText(/再次删除同一任务即可重试清理/)).toBeInTheDocument();
  });

  it('maps an unconfigured target to a configuration hint', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(errorResponse(409, 'handoff_unconfigured', 'handoff target is not configured')));
    renderDialog(fetchMock);

    fireEvent.click(screen.getByRole('button', { name: '移交并删除' }));

    await waitFor(() => expect(screen.getByText(/尚未配置移交目标/)).toBeInTheDocument());
  });
});
