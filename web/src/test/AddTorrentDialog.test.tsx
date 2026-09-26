import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { AddTorrentDialog } from '../components/dialogs/AddTorrentDialog';
import dialogStyles from '../components/dialogs/AddTorrentDialog.module.css';
import { theme } from '../styles/theme';
import type { Torrent } from '../types/api';

function torrent(id = 'torrent-1'): Torrent {
  return {
    id,
    info_hash: `abc123-${id}`,
    name: 'Example torrent',
    state: 'adding',
    total_bytes: 100,
    downloaded_bytes: 0,
    uploaded_bytes: 0,
    cached_bytes: 0,
    created_at: '2026-09-17T00:00:00Z',
    favorite: false,
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
  return requestAt(fetchMock, 0);
}

function requestAt(fetchMock: ReturnType<typeof vi.fn>, index: number): RequestInit {
  return (fetchMock.mock.calls as unknown as Array<[RequestInfo | URL, RequestInit]>)[index]?.[1] ?? {};
}

function file(name: string, lastModified = 1): File {
  return new File(['payload'], name, { type: 'application/x-bittorrent', lastModified });
}

describe('AddTorrentDialog', () => {
  it('sends a trimmed magnet payload and reports the new task', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(torrent())));
    const { onAdded } = renderDialog(fetchMock);

    fireEvent.change(screen.getByRole('textbox', { name: '磁力链接' }), { target: { value: '  magnet:?xt=urn:btih:abc  ' } });
    fireEvent.click(screen.getByRole('button', { name: '添加磁力链接' }));

    await waitFor(() => expect(onAdded).toHaveBeenCalledWith(['torrent-1']));
    const request = firstRequest(fetchMock);
    expect(JSON.parse(String(request.body))).toEqual({ magnet_uri: 'magnet:?xt=urn:btih:abc' });
    expect(new Headers(request.headers).get('Content-Type')).toBe('application/json');
  });

  it('uploads one selected torrent as multipart and completes with its task', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(torrent())));
    const { onAdded } = renderDialog(fetchMock);

    fireEvent.click(screen.getByRole('tab', { name: '种子文件' }));
    const selectedFile = file('sample.torrent');
    const input = document.querySelector('input[type="file"]');
    expect(input).not.toBeNull();
    fireEvent.change(input as HTMLInputElement, { target: { files: [selectedFile] } });
    fireEvent.click(screen.getByRole('button', { name: '上传种子文件' }));

    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('已处理 1 / 1'));
    const succeededStatus = screen.getByText('已处理，任务可用');
    expect(succeededStatus.parentElement).toHaveAttribute('data-status', 'succeeded');
    expect(succeededStatus.parentElement).toHaveClass(dialogStyles.fileStatus);
    expect(succeededStatus.parentElement?.querySelector(`.${dialogStyles.fileStatusError}`)).toBeNull();
    expect(fetchMock).toHaveBeenCalledOnce();
    const request = firstRequest(fetchMock);
    expect(request.body).toBeInstanceOf(FormData);
    expect(new Headers(request.headers).get('Content-Type')).toBeNull();
    expect((request.body as FormData).get('file')).toBe(selectedFile);

    fireEvent.click(screen.getByRole('button', { name: '完成并关闭' }));
    expect(onAdded).toHaveBeenCalledWith(['torrent-1']);
  });

  it('marks an uploading file without applying an error style', async () => {
    let resolveRequest!: (response: Response) => void;
    const fetchMock = vi.fn(() => new Promise<Response>((resolve) => { resolveRequest = resolve; }));
    renderDialog(fetchMock);

    fireEvent.click(screen.getByRole('tab', { name: '种子文件' }));
    const input = document.querySelector('input[type="file"]');
    fireEvent.change(input as HTMLInputElement, { target: { files: [file('pending.torrent')] } });
    fireEvent.click(screen.getByRole('button', { name: '上传种子文件' }));

    await waitFor(() => expect(screen.getByText('正在上传')).toBeInTheDocument());
    const uploadingStatus = screen.getByText('正在上传');
    expect(uploadingStatus.parentElement).toHaveAttribute('data-status', 'uploading');
    expect(uploadingStatus.parentElement).toHaveClass(dialogStyles.fileStatus);
    expect(uploadingStatus.parentElement?.querySelector(`.${dialogStyles.fileStatusError}`)).toBeNull();

    resolveRequest(jsonResponse(torrent()));
  });

  it('uploads multiple files in order and reports count progress', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(torrent())));
    renderDialog(fetchMock);

    fireEvent.click(screen.getByRole('tab', { name: '种子文件' }));
    const files = [file('one.torrent'), file('two.TORRENT')];
    const input = document.querySelector('input[type="file"]');
    fireEvent.change(input as HTMLInputElement, { target: { files } });
    expect(screen.getByText('文件队列（2）')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: '上传种子文件' }));

    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('已处理 2 / 2'));
    expect(screen.getByRole('status')).toHaveTextContent('2 个文件成功，1 个唯一任务可用');
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect((requestAt(fetchMock, 0).body as FormData).get('file')).toBe(files[0]);
    expect((requestAt(fetchMock, 1).body as FormData).get('file')).toBe(files[1]);
  });

  it('keeps valid dropped files and reports invalid and duplicate inputs', () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(torrent())));
    renderDialog(fetchMock);

    fireEvent.click(screen.getByRole('tab', { name: '种子文件' }));
    const selectedFile = file('sample.torrent');
    const dropzone = screen.getByTestId('torrent-dropzone');
    fireEvent.drop(dropzone, { dataTransfer: { files: [selectedFile, file('notes.txt')] } });
    expect(screen.getByText('文件队列（1）')).toBeInTheDocument();
    const queuedStatus = screen.getByText('等待上传');
    expect(queuedStatus.parentElement).toHaveAttribute('data-status', 'queued');
    expect(queuedStatus.parentElement?.querySelector(`.${dialogStyles.fileStatusError}`)).toBeNull();
    expect(screen.getByRole('alert')).toHaveTextContent('notes.txt');

    const input = document.querySelector('input[type="file"]');
    fireEvent.change(input as HTMLInputElement, { target: { files: [] } });
    expect(screen.getByText('文件队列（1）')).toBeInTheDocument();
    fireEvent.drop(dropzone, { dataTransfer: { files: [selectedFile] } });
    expect(screen.getByText('文件队列（1）')).toBeInTheDocument();
    expect(screen.getByRole('alert')).toHaveTextContent('重复文件');
  });

  it('continues after a file error and retries only the failed item', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse(torrent('torrent-1')))
      .mockResolvedValueOnce(jsonResponse({ error: 'too large' }, { status: 413 }))
      .mockResolvedValueOnce(jsonResponse(torrent('torrent-2')))
      .mockResolvedValueOnce(jsonResponse(torrent('torrent-3')));
    const { onAdded } = renderDialog(fetchMock);

    fireEvent.click(screen.getByRole('tab', { name: '种子文件' }));
    const files = [file('one.torrent'), file('two.torrent'), file('three.torrent')];
    const input = document.querySelector('input[type="file"]');
    fireEvent.change(input as HTMLInputElement, { target: { files } });
    fireEvent.click(screen.getByRole('button', { name: '上传种子文件' }));

    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('2 个文件成功，2 个唯一任务可用，1 个文件失败或未尝试'));
    expect(fetchMock).toHaveBeenCalledTimes(3);
    const fileError = screen.getByText('上传的 .torrent 文件超过后台服务的大小限制。');
    expect(fileError).toHaveClass(dialogStyles.fileStatusError);
    expect(fileError.parentElement).toHaveAttribute('data-status', 'failed');

    fireEvent.click(screen.getByRole('button', { name: '重试失败项' }));
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('已处理 1 / 1'));
    expect(fetchMock).toHaveBeenCalledTimes(4);
    expect((requestAt(fetchMock, 3).body as FormData).get('file')).toBe(files[1]);

    fireEvent.click(screen.getByRole('button', { name: '完成并关闭' }));
    expect(onAdded).toHaveBeenCalledWith(['torrent-1', 'torrent-3', 'torrent-2']);
  });

  it('stops after a connection error and marks remaining files untried', async () => {
    const fetchMock = vi.fn()
      .mockRejectedValueOnce(new TypeError('network down'))
      .mockResolvedValueOnce(jsonResponse(torrent('torrent-2')));
    renderDialog(fetchMock);

    fireEvent.click(screen.getByRole('tab', { name: '种子文件' }));
    const input = document.querySelector('input[type="file"]');
    fireEvent.change(input as HTMLInputElement, { target: { files: [file('one.torrent'), file('two.torrent')] } });
    fireEvent.click(screen.getByRole('button', { name: '上传种子文件' }));

    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('0 个文件成功，0 个唯一任务可用，2 个文件失败或未尝试'));
    expect(fetchMock).toHaveBeenCalledOnce();
    expect(screen.getByText('未尝试')).toBeInTheDocument();
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

  it('maps magnet request size errors without implying a file upload', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse({ error: 'payload too large' }, { status: 413 })));
    renderDialog(fetchMock);

    fireEvent.change(screen.getByRole('textbox', { name: '磁力链接' }), { target: { value: 'magnet:?xt=urn:btih:abc' } });
    fireEvent.click(screen.getByRole('button', { name: '添加磁力链接' }));

    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('磁力链接请求超过后台服务允许的大小限制。'));
    expect(screen.getByRole('alert')).not.toHaveTextContent('.torrent');
  });
});
