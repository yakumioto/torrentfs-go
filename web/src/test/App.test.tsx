import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from '../app/App';
import { AuthProvider } from '../app/auth';
import { queryKeys } from '../queries/keys';
import { theme } from '../styles/theme';

function jsonResponse(body: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
    ...init,
  });
}

function unauthorizedResponse(): Response {
  return jsonResponse({ error: 'unauthorized' }, {
    status: 401,
    headers: { 'WWW-Authenticate': 'Bearer' },
  });
}

function renderApp() {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0 },
    },
  });
  const view = render(
    <MantineProvider theme={theme} defaultColorScheme="dark">
      <QueryClientProvider client={queryClient}>
        <AuthProvider>
          <MemoryRouter>
            <App />
          </MemoryRouter>
        </AuthProvider>
      </QueryClientProvider>
    </MantineProvider>,
  );
  return { queryClient, ...view };
}

function mockFetch(...responses: Response[]) {
  const fetchMock = vi.fn<typeof fetch>(() => {
    const response = responses.shift();
    if (response === undefined) {
      throw new Error('Unexpected fetch request in test.');
    }
    return Promise.resolve(response);
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function authorization(request: RequestInit | undefined): string | null {
  return new Headers(request?.headers).get('Authorization');
}

async function submitLogin(username = 'alice', password = 'secret') {
  fireEvent.change(screen.getByRole('textbox', { name: 'Username' }), { target: { value: username } });
  const passwordInput = document.querySelector<HTMLInputElement>('input[type="password"]');
  if (passwordInput === null) {
    throw new Error('Password input is missing.');
  }
  fireEvent.change(passwordInput, { target: { value: password } });
  fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
}

beforeEach(() => {
  sessionStorage.clear();
  vi.stubGlobal('fetch', vi.fn());
});
afterEach(() => {
  cleanup();
  sessionStorage.clear();
  vi.unstubAllGlobals();
});

describe('App authentication flow', () => {
  it('renders an actionable login page instead of exposing an unauthorized probe response', async () => {
    const fetchMock = mockFetch(unauthorizedResponse());
    renderApp();

    await waitFor(() => expect(screen.getByRole('heading', { name: 'Sign in to TorrentFS' })).toBeInTheDocument());

    expect(screen.getByRole('status')).toHaveTextContent('Authentication is required or your session has expired. Sign in to continue.');
    expect(screen.queryByText('The daemon is out of reach')).not.toBeInTheDocument();
    expect(screen.queryByText('unauthorized')).not.toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledOnce();
  });

  it('restores a valid session after an authenticated page is remounted', async () => {
    const fetchMock = mockFetch(
      unauthorizedResponse(),
      jsonResponse({ token: 'opaque', token_type: 'Bearer', expires_in: 60 }),
      jsonResponse([]),
      jsonResponse([]),
      jsonResponse([]),
    );
    const firstMount = renderApp();

    await waitFor(() => expect(screen.getByRole('heading', { name: 'Sign in to TorrentFS' })).toBeInTheDocument());
    await submitLogin();
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Torrents' })).toBeInTheDocument());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));

    expect(authorization(fetchMock.mock.calls[1][1])).toBeNull();
    expect(authorization(fetchMock.mock.calls[2][1])).toBe('Bearer opaque');
    expect(sessionStorage.getItem('torrentfs.access-token')).toBe('opaque');

    firstMount.unmount();
    firstMount.queryClient.clear();
    renderApp();

    await waitFor(() => expect(screen.getByRole('heading', { name: 'Torrents' })).toBeInTheDocument());
    expect(screen.queryByText(/Sign in again/)).not.toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledTimes(5);
    expect(authorization(fetchMock.mock.calls[3][1])).toBe('Bearer opaque');
    expect(authorization(fetchMock.mock.calls[4][1])).toBe('Bearer opaque');
  });

  it('returns to the login page and clears protected data when a session request expires', async () => {
    const fetchMock = mockFetch(
      unauthorizedResponse(),
      jsonResponse({ token: 'opaque', token_type: 'Bearer', expires_in: 60 }),
      jsonResponse([]),
      unauthorizedResponse(),
    );
    const { queryClient } = renderApp();

    await waitFor(() => expect(screen.getByRole('heading', { name: 'Sign in to TorrentFS' })).toBeInTheDocument());
    await submitLogin();
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Torrents' })).toBeInTheDocument());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));

    fireEvent.click(screen.getByRole('button', { name: 'Refresh data' }));

    await waitFor(() => expect(screen.getByRole('heading', { name: 'Sign in to TorrentFS' })).toBeInTheDocument());
    await waitFor(() => expect(queryClient.getQueryData(queryKeys.torrents)).toBeUndefined());
    expect(screen.getByRole('status')).toHaveTextContent('Authentication is required or your session has expired. Sign in to continue.');
    expect(screen.queryByText('unauthorized')).not.toBeInTheDocument();
    expect(sessionStorage.getItem('torrentfs.access-token')).toBeNull();
    expect(authorization(fetchMock.mock.calls[3][1])).toBe('Bearer opaque');
  });

  it('does not show a session notice after intentional logout and allows signing in again', async () => {
    const fetchMock = mockFetch(
      unauthorizedResponse(),
      jsonResponse({ token: 'opaque', token_type: 'Bearer', expires_in: 60 }),
      jsonResponse([]),
      new Response(null, { status: 204 }),
      unauthorizedResponse(),
      jsonResponse({ token: 'opaque-new', token_type: 'Bearer', expires_in: 60 }),
      jsonResponse([]),
    );
    renderApp();

    await waitFor(() => expect(screen.getByRole('heading', { name: 'Sign in to TorrentFS' })).toBeInTheDocument());
    await submitLogin();
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Torrents' })).toBeInTheDocument());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));

    fireEvent.click(screen.getByRole('button', { name: 'Log out' }));

    await waitFor(() => expect(screen.getByRole('heading', { name: 'Sign in to TorrentFS' })).toBeInTheDocument());
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
    expect(screen.queryByText('Authentication is required or your session has expired. Sign in to continue.')).not.toBeInTheDocument();
    expect(sessionStorage.getItem('torrentfs.access-token')).toBeNull();
    expect(authorization(fetchMock.mock.calls[3][1])).toBe('Bearer opaque');
    expect(authorization(fetchMock.mock.calls[4][1])).toBeNull();

    await submitLogin();
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Torrents' })).toBeInTheDocument());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(7));
    expect(authorization(fetchMock.mock.calls[5][1])).toBeNull();
    expect(authorization(fetchMock.mock.calls[6][1])).toBe('Bearer opaque-new');
  });

  it('keeps invalid credentials separate from the session notice', async () => {
    const fetchMock = mockFetch(unauthorizedResponse(), unauthorizedResponse());
    renderApp();

    await waitFor(() => expect(screen.getByRole('heading', { name: 'Sign in to TorrentFS' })).toBeInTheDocument());
    await submitLogin('alice', 'wrong');

    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Invalid username or password.'));
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(authorization(fetchMock.mock.calls[1][1])).toBeNull();
  });
});
