import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { CategoryManagementDialog } from '../components/dialogs/CategoryManagementDialog';
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
    <MantineProvider theme={theme} defaultColorScheme="light">
      <QueryClientProvider client={queryClient}>
        <AuthContext.Provider value={auth}>
          <CategoryManagementDialog opened onClose={vi.fn()} />
        </AuthContext.Provider>
      </QueryClientProvider>
    </MantineProvider>,
  );
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('category management dialog', () => {
  it('creates a category and immediately shows the result', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (init?.method === 'POST') {
        return Promise.resolve(jsonResponse({ name: 'movies', created_at: '2026-09-28T08:00:00Z' }, 201));
      }
      if (url.endsWith('/categories')) {
        return Promise.resolve(jsonResponse([]));
      }
      return Promise.resolve(jsonResponse([]));
    });
    renderDialog(fetchMock);

    await waitFor(() => expect(screen.getByText('还没有分类。')).toBeInTheDocument());
    fireEvent.change(screen.getByRole('textbox', { name: '新建分类' }), { target: { value: 'movies' } });
    fireEvent.click(screen.getByRole('button', { name: '创建分类' }));

    await waitFor(() => expect(screen.getByText('movies')).toBeInTheDocument());
    expect(fetchMock.mock.calls.some(([input, init]) => String(input).endsWith('/categories') && init?.method === 'POST')).toBe(true);
    const post = fetchMock.mock.calls.find(([, init]) => init?.method === 'POST');
    expect((post?.[1] as RequestInit).body).toBe(JSON.stringify({ name: 'movies' }));
  });
});
