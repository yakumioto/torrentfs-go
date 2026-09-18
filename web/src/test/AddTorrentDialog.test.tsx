import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { AddTorrentDialog } from '../components/dialogs/AddTorrentDialog';
import { theme } from '../styles/theme';
import type { Torrent } from '../types/api';

function torrent(): Torrent {
  return {
    id: 'torrent-1',
    info_hash: 'abc123',
    name: 'Example torrent',
    state: 'adding',
    total_bytes: 100,
    cached_bytes: 0,
    created_at: '2026-09-17T00:00:00Z',
  };
}

function jsonResponse(body: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' }, ...init });
}

function renderDialog(fetchMock: ReturnType<typeof vi.fn>, onAdded = vi.fn(), onClose = vi.fn()) {
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
          <AddTorrentDialog opened onClose={onClose} api={api} onAdded={onAdded} />
        </AuthContext.Provider>
      </QueryClientProvider>
    </MantineProvider>,
  );
  return { onAdded, onClose };
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function firstRequest(fetchMock: ReturnType<typeof vi.fn>): RequestInit {
  return (fetchMock.mock.calls as unknown as Array<[RequestInfo | URL, RequestInit]>)[0]?.[1] ?? {};
}

describe('AddTorrentDialog', () => {
  it('sends a trimmed magnet payload and reports the new task', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(torrent())));
    const { onAdded } = renderDialog(fetchMock);

    fireEvent.change(screen.getByRole('textbox', { name: '磁力链接' }), { target: { value: '  magnet:?xt=urn:btih:abc  ' } });
    fireEvent.click(screen.getByRole('button', { name: '添加磁力链接' }));

    await waitFor(() => expect(onAdded).toHaveBeenCalledWith('torrent-1'));
    const request = firstRequest(fetchMock);
    expect(JSON.parse(String(request.body))).toEqual({ magnet_uri: 'magnet:?xt=urn:btih:abc' });
    expect(new Headers(request.headers).get('Content-Type')).toBe('application/json');
  });

  it('uploads a torrent file as multipart without overriding its boundary', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(torrent())));
    const { onAdded } = renderDialog(fetchMock);

    fireEvent.click(screen.getByRole('tab', { name: '种子文件' }));
    const file = new File(['payload'], 'sample.torrent', { type: 'application/x-bittorrent' });
    const input = document.querySelector('input[type="file"]');
    expect(input).not.toBeNull();
    fireEvent.change(input as HTMLInputElement, { target: { files: [file] } });
    fireEvent.click(screen.getByRole('button', { name: '上传种子文件' }));

    await waitFor(() => expect(onAdded).toHaveBeenCalledWith('torrent-1'));
    const request = firstRequest(fetchMock);
    expect(request.body).toBeInstanceOf(FormData);
    expect(new Headers(request.headers).get('Content-Type')).toBeNull();
  });

  it('maps upload limit errors to Chinese copy', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse({ error: 'payload too large' }, { status: 413 })));
    renderDialog(fetchMock);

    fireEvent.change(screen.getByRole('textbox', { name: '磁力链接' }), { target: { value: 'magnet:?xt=urn:btih:abc' } });
    fireEvent.click(screen.getByRole('button', { name: '添加磁力链接' }));

    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('上传的 .torrent 文件超过后台服务的大小限制。'));
  });

  it('cannot be closed while the add request is pending', async () => {
    let resolveRequest!: (response: Response) => void;
    const fetchMock = vi.fn(() => new Promise<Response>((resolve) => { resolveRequest = resolve; }));
    const { onClose } = renderDialog(fetchMock);

    fireEvent.change(screen.getByRole('textbox', { name: '磁力链接' }), { target: { value: 'magnet:?xt=urn:btih:abc' } });
    fireEvent.click(screen.getByRole('button', { name: '添加磁力链接' }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledOnce());
    fireEvent.click(screen.getByRole('button', { name: '关闭弹窗' }));
    expect(onClose).not.toHaveBeenCalled();

    resolveRequest(jsonResponse(torrent()));
  });
});
