import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { DeleteSubtitleDialog } from '../components/dialogs/DeleteSubtitleDialog';
import { theme } from '../styles/theme';
import type { Subtitle } from '../types/api';

const SUBTITLE: Subtitle = {
  video_path: 'Movie.2026.mkv',
  path: 'Movie.2026.srt',
  mount_path: 'Show/Movie.2026.srt',
  format: 'srt',
  size: 4,
  updated_at: '2026-09-26T12:00:00Z',
};

function renderDialog(options: { fetchMock: ReturnType<typeof vi.fn>; onDeleted?: () => void; onClose?: () => void }) {
  const api = new ApiClient();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } } });
  vi.stubGlobal('fetch', options.fetchMock);
  render(
    <QueryClientProvider client={queryClient}>
      <MantineProvider theme={theme} defaultColorScheme="light">
        <DeleteSubtitleDialog
          api={api}
          torrentId="torrent-1"
          subtitle={SUBTITLE}
          opened
          onClose={options.onClose ?? (() => undefined)}
          onDeleted={options.onDeleted}
        />
      </MantineProvider>
    </QueryClientProvider>,
  );
}

function deleteCall(fetchMock: ReturnType<typeof vi.fn>) {
  return fetchMock.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === 'DELETE');
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('DeleteSubtitleDialog', () => {
  it('names the mount path and deletes only that subtitle', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    const onDeleted = vi.fn();
    renderDialog({ fetchMock, onDeleted });

    expect(screen.getByText('Show/Movie.2026.srt')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: '删除字幕' }));

    await waitFor(() => expect(onDeleted).toHaveBeenCalledTimes(1));
    const call = deleteCall(fetchMock);
    expect(String(call?.[0])).toBe('/api/v1/torrents/torrent-1/subtitles?path=Movie.2026.srt');
    const request = call?.[1] as RequestInit;
    expect(request.method).toBe('DELETE');
    expect(request.body).toBeUndefined();
  });

  it('keeps the dialog open and explains a torrent that is being deleted', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(
      JSON.stringify({ error: '任务正在删除，暂时不能上传字幕。', code: 'torrent_deleting' }),
      { status: 409, headers: { 'Content-Type': 'application/json' } },
    ));
    const onDeleted = vi.fn();
    renderDialog({ fetchMock, onDeleted });

    fireEvent.click(screen.getByRole('button', { name: '删除字幕' }));

    expect(await screen.findByText(/任务正在删除，暂时不能修改字幕/)).toBeInTheDocument();
    expect(onDeleted).not.toHaveBeenCalled();
  });
});
