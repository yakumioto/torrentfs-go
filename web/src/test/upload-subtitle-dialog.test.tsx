import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { UploadSubtitleDialog } from '../components/dialogs/UploadSubtitleDialog';
import { theme } from '../styles/theme';
import type { Subtitle, SubtitleTarget } from '../types/api';

function jsonResponse(body: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' }, ...init });
}

const TARGETS: SubtitleTarget[] = [
  { video_path: 'Movie.2026.mkv', mount_path: 'Show/Movie.2026.srt', expected_basename: 'Movie.2026', uploadable: true },
  { video_path: 'Season 1/E01.mp4', mount_path: 'Show/Season 1/E01.srt', expected_basename: 'E01', uploadable: true },
];

function renderDialog(options: {
  fetchMock: ReturnType<typeof vi.fn>;
  targets?: SubtitleTarget[];
  subtitles?: Subtitle[];
  onUploaded?: (result: { replaced: boolean; path: string; mountPath: string }) => void;
}) {
  const api = new ApiClient();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } } });
  vi.stubGlobal('fetch', options.fetchMock);
  render(
    <QueryClientProvider client={queryClient}>
      <MantineProvider theme={theme} defaultColorScheme="light">
        <UploadSubtitleDialog
          api={api}
          torrentId="torrent-1"
          targets={options.targets ?? TARGETS}
          subtitles={options.subtitles ?? []}
          opened
          onClose={() => undefined}
          onUploaded={options.onUploaded}
        />
      </MantineProvider>
    </QueryClientProvider>,
  );
  return queryClient;
}

// Mantine's FileInput keeps the real input hidden behind its own wrapper, so
// the file is selected through the input element itself.
function fileInput(): HTMLInputElement {
  const input = document.querySelector('input[type="file"]');
  if (input === null) {
    throw new Error('no file input rendered');
  }
  return input as HTMLInputElement;
}

function uploadCall(fetchMock: ReturnType<typeof vi.fn>) {
  return fetchMock.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === 'PUT');
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('UploadSubtitleDialog', () => {
  it('auto-selects the only uploadable target and previews the destination paths', () => {
    renderDialog({ fetchMock: vi.fn(), targets: [TARGETS[0]] });
    expect(screen.getByText('Show/Movie.2026.srt')).toBeInTheDocument();
    expect(screen.getByText('Movie.2026.srt')).toBeInTheDocument();
    expect((screen.getByLabelText('目标视频') as HTMLSelectElement).value).toBe('Movie.2026.mkv');
  });

  it('requires an explicit choice when several videos are uploadable', async () => {
    renderDialog({ fetchMock: vi.fn() });
    const select = screen.getByLabelText('目标视频') as HTMLSelectElement;
    expect(select.value).toBe('');
    expect(screen.getByRole('button', { name: '上传字幕' })).toBeDisabled();
    expect(screen.queryByText('Show/Season 1/E01.srt')).not.toBeInTheDocument();

    fireEvent.change(select, { target: { value: 'Season 1/E01.mp4' } });
    expect(await screen.findByText('Show/Season 1/E01.srt')).toBeInTheDocument();
  });

  it('rejects a mismatched basename and a non-lowercase extension before upload', async () => {
    const fetchMock = vi.fn();
    renderDialog({ fetchMock, targets: [TARGETS[0]] });

    fireEvent.change(fileInput(), { target: { files: [new File(['x'], 'other.srt')] } });
    expect(await screen.findByRole('alert')).toHaveTextContent('字幕文件名必须与所选视频的名称完全一致。');
    expect(screen.getByRole('button', { name: '上传字幕' })).toBeDisabled();

    fireEvent.change(fileInput(), { target: { files: [new File(['x'], 'Movie.2026.SRT')] } });
    expect(await screen.findByRole('alert')).toHaveTextContent('仅支持小写扩展名');
    expect(screen.getByRole('button', { name: '上传字幕' })).toBeDisabled();

    fireEvent.change(fileInput(), { target: { files: [new File(['x'], 'Movie.2026.srt')] } });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: '上传字幕' })).toBeEnabled();
    expect(uploadCall(fetchMock)).toBeUndefined();
  });

  it('uploads the selected video and reports the created subtitle', async () => {
    const uploaded = vi.fn();
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse({
      torrent_id: 'torrent-1',
      video_path: 'Movie.2026.mkv',
      path: 'Movie.2026.srt',
      mount_path: 'Show/Movie.2026.srt',
      format: 'srt',
      size: 4,
      updated_at: '2026-09-26T12:00:00Z',
      replaced: false,
    }, { status: 201 })));
    renderDialog({ fetchMock, targets: [TARGETS[0]], onUploaded: uploaded });

    fireEvent.change(fileInput(), { target: { files: [new File(['sub!'], 'Movie.2026.srt')] } });
    fireEvent.click(screen.getByRole('button', { name: '上传字幕' }));

    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('字幕已上传'));
    const call = uploadCall(fetchMock);
    expect(String(call?.[0])).toBe('/api/v1/torrents/torrent-1/subtitles');
    const body = (call?.[1] as RequestInit).body as FormData;
    expect(body.get('video_path')).toBe('Movie.2026.mkv');
    expect(uploaded).toHaveBeenCalledWith({ replaced: false, path: 'Movie.2026.srt', mountPath: 'Show/Movie.2026.srt' });
  });

  it('warns about a replacement and changes the submit label', async () => {
    const fetchMock = vi.fn();
    renderDialog({
      fetchMock,
      targets: [TARGETS[0]],
      subtitles: [{ video_path: 'Movie.2026.mkv', path: 'Movie.2026.srt', mount_path: 'Show/Movie.2026.srt', format: 'srt', size: 3, updated_at: '2026-09-26T10:00:00Z' }],
    });
    fireEvent.change(fileInput(), { target: { files: [new File(['x'], 'Movie.2026.srt')] } });
    expect(await screen.findByText('将替换现有字幕')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '替换字幕' })).toBeEnabled();
  });

  it('reports a stable-code failure and allows a retry', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse({ error: '目标路径属于种子自带文件，不能覆盖。', code: 'subtitle_payload_conflict' }, { status: 409, statusText: 'Conflict' }))
      .mockResolvedValueOnce(jsonResponse({
        torrent_id: 'torrent-1', video_path: 'Movie.2026.mkv', path: 'Movie.2026.srt', mount_path: 'Show/Movie.2026.srt',
        format: 'srt', size: 4, updated_at: '2026-09-26T12:00:00Z', replaced: false,
      }, { status: 201 }));
    renderDialog({ fetchMock, targets: [TARGETS[0]] });

    fireEvent.change(fileInput(), { target: { files: [new File(['sub!'], 'Movie.2026.srt')] } });
    fireEvent.click(screen.getByRole('button', { name: '上传字幕' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('不会覆盖原始内容');
    fireEvent.click(screen.getByRole('button', { name: '上传字幕' }));
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('字幕已上传'));
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('invalidates only this torrent status after a successful upload', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse({
      torrent_id: 'torrent-1', video_path: 'Movie.2026.mkv', path: 'Movie.2026.srt', mount_path: 'Show/Movie.2026.srt',
      format: 'srt', size: 4, updated_at: '2026-09-26T12:00:00Z', replaced: false,
    }, { status: 201 })));
    const queryClient = renderDialog({ fetchMock, targets: [TARGETS[0]] });
    const invalidate = vi.spyOn(queryClient, 'invalidateQueries');

    fireEvent.change(fileInput(), { target: { files: [new File(['sub!'], 'Movie.2026.srt')] } });
    fireEvent.click(screen.getByRole('button', { name: '上传字幕' }));

    await waitFor(() => expect(invalidate).toHaveBeenCalled());
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['torrent-status', 'torrent-1'] });
    expect(invalidate).not.toHaveBeenCalledWith({ queryKey: ['torrents'] });
  });

  it('explains a disabled target list', () => {
    renderDialog({
      fetchMock: vi.fn(),
      targets: [{ video_path: 'movie.mkv', mount_path: 'Show/movie.srt', expected_basename: 'movie', uploadable: false, reason: 'subtitle_name_conflict' }],
    });
    expect(screen.getByText(/这个任务没有可上传字幕的视频/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '上传字幕' })).toBeDisabled();
    expect(screen.getByLabelText('目标视频')).toBeDisabled();
  });
});
