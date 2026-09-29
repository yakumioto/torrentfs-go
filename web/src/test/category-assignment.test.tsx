import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { CategoryAssignmentDialog } from '../components/dialogs/CategoryAssignmentDialog';
import type { Torrent } from '../types/api';
import { theme } from '../styles/theme';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

const torrent: Torrent = {
  id: 'torrent-1',
  info_hash: 'abc123',
  name: 'Movie1.mp4',
  category: '',
  state: 'ready',
  total_bytes: 100,
  downloaded_bytes: 0,
  uploaded_bytes: 0,
  cached_bytes: 0,
  created_at: '2026-09-28T08:00:00Z',
  favorite: false,
};

function renderDialog(fetchMock: ReturnType<typeof vi.fn>, torrentValue: Torrent = torrent) {
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
          <CategoryAssignmentDialog torrent={torrentValue} opened onClose={vi.fn()} />
        </AuthContext.Provider>
      </QueryClientProvider>
    </MantineProvider>,
  );
}

beforeEach(() => {
  vi.unstubAllGlobals();
  vi.stubGlobal('ResizeObserver', class {
    observe() {}
    unobserve() {}
    disconnect() {}
  });
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('category assignment dialog', () => {
  it('sends the selected category and returns the updated torrent', async () => {
    const updated = { ...torrent, category: 'movies' };
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (init?.method === 'PUT') {
        return Promise.resolve(jsonResponse(updated));
      }
      if (url.endsWith('/categories')) {
        return Promise.resolve(jsonResponse([{ name: 'movies', created_at: '2026-09-28T08:00:00Z' }]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderDialog(fetchMock);

    await waitFor(() => expect(screen.getByRole('combobox', { name: '所属分类' })).not.toBeDisabled());
    fireEvent.change(screen.getByRole('combobox', { name: '所属分类' }), { target: { value: 'movies' } });
    fireEvent.click(screen.getByRole('button', { name: '保存分类' }));

    await waitFor(() => expect(fetchMock.mock.calls.some(([input, init]) => String(input).endsWith('/category') && init?.method === 'PUT')).toBe(true));
    const put = fetchMock.mock.calls.find(([input, init]) => String(input).endsWith('/category') && init?.method === 'PUT');
    expect((put?.[1] as RequestInit).body).toBe(JSON.stringify({ category: 'movies' }));
  });

  it('submits a legal __unclassified__ category without treating it as the unclassified option', async () => {
    const category = '__unclassified__';
    const updated = { ...torrent, category };
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (init?.method === 'PUT') {
        return Promise.resolve(jsonResponse(updated));
      }
      if (url.endsWith('/categories')) {
        return Promise.resolve(jsonResponse([{ name: category, created_at: '2026-09-28T08:00:00Z' }]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderDialog(fetchMock);

    await waitFor(() => expect(screen.getByRole('combobox', { name: '所属分类' })).not.toBeDisabled());
    fireEvent.change(screen.getByRole('combobox', { name: '所属分类' }), { target: { value: category } });
    fireEvent.click(screen.getByRole('button', { name: '保存分类' }));

    await waitFor(() => expect(fetchMock.mock.calls.some(([input, init]) => String(input).endsWith('/category') && init?.method === 'PUT')).toBe(true));
    const put = fetchMock.mock.calls.find(([input, init]) => String(input).endsWith('/category') && init?.method === 'PUT');
    expect((put?.[1] as RequestInit).body).toBe(JSON.stringify({ category }));
  });

  it('submits an empty category when restoring an assigned torrent to unclassified', async () => {
    const category = '__unclassified__';
    const assigned = { ...torrent, category };
    const updated = { ...torrent, category: '' };
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (init?.method === 'PUT') {
        return Promise.resolve(jsonResponse(updated));
      }
      if (url.endsWith('/categories')) {
        return Promise.resolve(jsonResponse([{ name: category, created_at: '2026-09-28T08:00:00Z' }]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderDialog(fetchMock, assigned);

    await waitFor(() => expect(screen.getByRole('combobox', { name: '所属分类' })).not.toBeDisabled());
    fireEvent.change(screen.getByRole('combobox', { name: '所属分类' }), { target: { value: '' } });
    fireEvent.click(screen.getByRole('button', { name: '保存分类' }));

    await waitFor(() => expect(fetchMock.mock.calls.some(([input, init]) => String(input).endsWith('/category') && init?.method === 'PUT')).toBe(true));
    const put = fetchMock.mock.calls.find(([input, init]) => String(input).endsWith('/category') && init?.method === 'PUT');
    expect((put?.[1] as RequestInit).body).toBe(JSON.stringify({ category: '' }));
  });
});
