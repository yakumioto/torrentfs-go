import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { MAX_PRUNE_DAYS, PruneTorrentsDialog } from '../components/dialogs/PruneTorrentsDialog';
import { theme } from '../styles/theme';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

function renderDialog(fetchMock: ReturnType<typeof vi.fn>) {
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
          <PruneTorrentsDialog opened onClose={() => undefined} />
        </AuthContext.Provider>
      </MantineProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('PruneTorrentsDialog', () => {
  it('states that favorites are exempt before anything is submitted', () => {
    renderDialog(vi.fn(() => Promise.resolve(jsonResponse({ operations: [], excluded_favorites: 0 }))));
    expect(screen.getByRole('dialog')).toHaveTextContent('已收藏的任务不会被删除');
  });

  it('posts the requested threshold and reports how many favorites were kept', async () => {
    const fetchMock: ReturnType<typeof vi.fn> = vi.fn(() => Promise.resolve(jsonResponse({
      operations: [{ operation_id: 'op-1', torrent_id: 'abc', state: 'deleting' }],
      excluded_favorites: 2,
    })));
    renderDialog(fetchMock);

    const input = screen.getByLabelText('保留天数阈值');
    fireEvent.change(input, { target: { value: '7' } });
    fireEvent.click(screen.getByRole('button', { name: '开始清理' }));

    await waitFor(() => expect(screen.getByText('已开始删除 1 个任务。')).toBeInTheDocument());
    expect(screen.getByText('已保留 2 个收藏任务。')).toBeInTheDocument();

    const call = fetchMock.mock.calls[0];
    expect(String(call[0])).toBe('/api/v1/torrents/prune');
    const request = call[1] as RequestInit;
    expect(request.method).toBe('POST');
    expect(request.body).toBe(JSON.stringify({ older_than_days: 7 }));
  });

  it('refuses a threshold below one day', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse({ operations: [], excluded_favorites: 0 })));
    renderDialog(fetchMock);

    const input = screen.getByLabelText('保留天数阈值');
    fireEvent.change(input, { target: { value: '0' } });
    expect(screen.getByRole('button', { name: '开始清理' })).toBeDisabled();

    fireEvent.click(screen.getByRole('button', { name: '开始清理' }));
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('reports when nothing matched the threshold', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse({ operations: [], excluded_favorites: 0 })));
    renderDialog(fetchMock);

    fireEvent.click(screen.getByRole('button', { name: '开始清理' }));
    await waitFor(() => expect(screen.getByText('没有符合条件的任务。')).toBeInTheDocument());
  });

  it('refuses a threshold above the day limit before sending it', () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse({ operations: [], excluded_favorites: 0 })));
    renderDialog(fetchMock);

    const input = screen.getByLabelText('保留天数阈值');
    fireEvent.change(input, { target: { value: String(MAX_PRUNE_DAYS + 1) } });

    expect(screen.getByRole('button', { name: '开始清理' })).toBeDisabled();
    fireEvent.click(screen.getByRole('button', { name: '开始清理' }));
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
