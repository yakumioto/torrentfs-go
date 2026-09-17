import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { DeleteTorrentDialog } from '../components/dialogs/DeleteTorrentDialog';
import { theme } from '../styles/theme';
import type { Operation, Torrent } from '../types/api';

const PURGE_LABEL = 'Also purge managed payload data';
const CONFIRM_LABEL = 'I understand that only payload data managed by TorrentFS will be removed.';

function jsonResponse(body: unknown): Response {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
}

function makeTorrent(): Torrent {
  return {
    id: 'torrent-1',
    info_hash: 'abc123',
    name: 'Example torrent',
    state: 'downloading',
    total_bytes: 100,
    completed_bytes: 25,
    progress: 0.25,
    created_at: '2026-09-17T00:00:00Z',
  };
}

function renderDialog({ initialPurgeData, fetchMock }: { initialPurgeData: boolean; fetchMock: ReturnType<typeof vi.fn> }) {
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
      <MantineProvider theme={theme} defaultColorScheme="dark">
        <AuthContext.Provider value={auth}>
          <DeleteTorrentDialog torrent={makeTorrent()} opened initialPurgeData={initialPurgeData} onClose={() => undefined} />
        </AuthContext.Provider>
      </MantineProvider>
    </QueryClientProvider>,
  );
}

function deletionUrl(fetchMock: ReturnType<typeof vi.fn>): string {
  const call = fetchMock.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === 'DELETE');
  return call === undefined ? '' : String(call[0]);
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('DeleteTorrentDialog purge confirmation', () => {
  it('keeps the explicit second confirmation before a purge delete is submitted', async () => {
    const operation: Operation = { operation_id: 'op-1', torrent_id: 'torrent-1', state: 'deleting', purge_data: true };
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(operation)));
    renderDialog({ initialPurgeData: true, fetchMock });

    const purge = screen.getByRole('checkbox', { name: PURGE_LABEL });
    expect(purge).toBeChecked();
    const confirm = screen.getByRole('checkbox', { name: CONFIRM_LABEL });
    expect(confirm).not.toBeChecked();

    const submit = screen.getByRole('button', { name: 'Confirm purge' });
    expect(submit).toBeDisabled();
    fireEvent.click(submit);
    expect(fetchMock).not.toHaveBeenCalled();

    fireEvent.click(confirm);
    expect(submit).toBeEnabled();
    fireEvent.click(submit);

    await waitFor(() => expect(deletionUrl(fetchMock)).not.toBe(''));
    expect(deletionUrl(fetchMock)).toMatch(/\/api\/v1\/torrents\/torrent-1\?purge_data=true$/);
  });

  it('drops the purge confirmation again once purge is toggled', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse({ operation_id: 'op-1', torrent_id: 'torrent-1', state: 'deleting', purge_data: true } as Operation)));
    renderDialog({ initialPurgeData: true, fetchMock });

    fireEvent.click(screen.getByRole('checkbox', { name: CONFIRM_LABEL }));
    expect(screen.getByRole('button', { name: 'Confirm purge' })).toBeEnabled();

    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_LABEL }));
    fireEvent.click(screen.getByRole('checkbox', { name: PURGE_LABEL }));

    expect(screen.getByRole('checkbox', { name: CONFIRM_LABEL })).not.toBeChecked();
    expect(screen.getByRole('button', { name: 'Confirm purge' })).toBeDisabled();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('leaves a plain delete available without the purge confirmation', async () => {
    const operation: Operation = { operation_id: 'op-1', torrent_id: 'torrent-1', state: 'deleting', purge_data: false };
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(operation)));
    renderDialog({ initialPurgeData: false, fetchMock });

    expect(screen.getByRole('checkbox', { name: PURGE_LABEL })).not.toBeChecked();
    expect(screen.queryByRole('checkbox', { name: CONFIRM_LABEL })).not.toBeInTheDocument();

    const submit = screen.getByRole('button', { name: 'Delete torrent' });
    expect(submit).toBeEnabled();
    fireEvent.click(submit);

    await waitFor(() => expect(deletionUrl(fetchMock)).not.toBe(''));
    expect(deletionUrl(fetchMock)).toMatch(/\/api\/v1\/torrents\/torrent-1$/);
  });
});
