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

function runtimeStatsResponse(): Response {
  return jsonResponse({
    started_at: '2026-09-23T09:00:00Z',
    cache: { used_bytes: 1, capacity_bytes: 2 },
    transfer: { downloaded_bytes: 3, uploaded_bytes: 4 },
  });
}

function disabledLabResponse(): Response {
  return jsonResponse({
    enabled: false,
    settings: null,
    seed: 0,
    announce_log: '',
    peer_sample_interval_seconds: 0,
    totals: { real_uploaded_bytes: 0, reported_uploaded_bytes: 0, overreported_bytes: 0 },
    torrents: [],
  });
}

function loginResponse(token: string): Response {
  return jsonResponse({ token, token_type: 'Bearer', expires_in: 60 });
}

function renderApp() {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0 },
    },
  });
  const view = render(
    <MantineProvider theme={theme} defaultColorScheme="light">
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

/**
 * URL-aware fetch double. The authenticated dashboard now issues three
 * independent queries (list, runtime stats, lab status), so a fixed response
 * sequence would make these tests depend on query start order.
 */
function mockFetch(handler: (url: string, request?: RequestInit) => Response | Promise<Response>) {
  const fetchMock = vi.fn<typeof fetch>((input, init) => Promise.resolve(handler(String(input), init)));
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function authorization(request: RequestInit | undefined): string | null {
  return new Headers(request?.headers).get('Authorization');
}

/** Dashboard endpoints shared by the authenticated-session cases. */
function dashboardHandler(options: {
  list: (request: RequestInit | undefined) => Response;
  login?: (request?: RequestInit) => Response;
}) {
  return (url: string, request?: RequestInit): Response => {
    if (url.endsWith('/auth/login')) {
      return options.login?.(request) ?? loginResponse('opaque');
    }
    if (url.endsWith('/torrents')) {
      return options.list(request);
    }
    if (url.endsWith('/stats')) {
      return runtimeStatsResponse();
    }
    if (url.endsWith('/lab')) {
      return disabledLabResponse();
    }
    throw new Error(`Unexpected request: ${url}`);
  };
}

async function submitLogin(password = 'secret') {
  const passwordInput = document.querySelector<HTMLInputElement>('input[type="password"]');
  if (passwordInput === null) {
    throw new Error('Password input is missing.');
  }
  fireEvent.change(passwordInput, { target: { value: password } });
  fireEvent.click(screen.getByRole('button', { name: '登录' }));
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
  it('renders a password-only login page instead of exposing an unauthorized probe response', async () => {
    const fetchMock = mockFetch(unauthorizedResponse);
    renderApp();

    await waitFor(() => expect(screen.getByRole('heading', { name: '登录 TorrentFS' })).toBeInTheDocument());

    expect(screen.queryByRole('textbox', { name: '用户名' })).not.toBeInTheDocument();
    expect(document.querySelector('input[type="password"]')).toBeRequired();
    expect(screen.getByRole('status')).toHaveTextContent('需要登录或当前会话已过期，请重新登录后继续。');
    expect(screen.queryByText('无法连接 TorrentFS 服务')).not.toBeInTheDocument();
    expect(screen.queryByText('unauthorized')).not.toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledOnce();
  });

  it('does not submit an empty password', async () => {
    const fetchMock = mockFetch(unauthorizedResponse);
    renderApp();

    await waitFor(() => expect(screen.getByRole('heading', { name: '登录 TorrentFS' })).toBeInTheDocument());
    await submitLogin('');

    expect(document.querySelector('input[type="password"]')).toBeInvalid();
    expect(fetchMock).toHaveBeenCalledOnce();
    expect(screen.getByRole('heading', { name: '登录 TorrentFS' })).toBeInTheDocument();
  });

  it('restores a valid session after an authenticated page is remounted', async () => {
    const fetchMock = mockFetch(dashboardHandler({
      list: (request) => (authorization(request) === null ? unauthorizedResponse() : jsonResponse([])),
    }));
    const firstMount = renderApp();

    await waitFor(() => expect(screen.getByRole('heading', { name: '登录 TorrentFS' })).toBeInTheDocument());
    await submitLogin();
    await waitFor(() => expect(screen.getByRole('heading', { name: '任务列表' })).toBeInTheDocument());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(5));

    expect(authorization(fetchMock.mock.calls[1][1])).toBeNull();
    expect(authorization(fetchMock.mock.calls[2][1])).toBe('Bearer opaque');
    expect(sessionStorage.getItem('torrentfs.access-token')).toBe('opaque');

    firstMount.unmount();
    firstMount.queryClient.clear();
    renderApp();

    await waitFor(() => expect(screen.getByRole('heading', { name: '任务列表' })).toBeInTheDocument());
    expect(screen.queryByText(/重新登录/)).not.toBeInTheDocument();
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(9));
    expect(authorization(fetchMock.mock.calls[5][1])).toBe('Bearer opaque');
    expect(authorization(fetchMock.mock.calls[6][1])).toBe('Bearer opaque');
  });

  it('returns to the login page and clears protected data when a session request expires', async () => {
    let authorizedListCalls = 0;
    const fetchMock = mockFetch(dashboardHandler({
      list: (request) => {
        if (authorization(request) === null) {
          return unauthorizedResponse();
        }
        authorizedListCalls += 1;
        return authorizedListCalls === 1 ? jsonResponse([]) : unauthorizedResponse();
      },
    }));
    const { queryClient } = renderApp();

    await waitFor(() => expect(screen.getByRole('heading', { name: '登录 TorrentFS' })).toBeInTheDocument());
    await submitLogin();
    await waitFor(() => expect(screen.getByRole('heading', { name: '任务列表' })).toBeInTheDocument());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(5));

    fireEvent.click(screen.getByRole('button', { name: '刷新数据' }));

    await waitFor(() => expect(screen.getByRole('heading', { name: '登录 TorrentFS' })).toBeInTheDocument());
    await waitFor(() => expect(queryClient.getQueryData(queryKeys.torrents)).toBeUndefined());
    expect(screen.getByRole('status')).toHaveTextContent('需要登录或当前会话已过期，请重新登录后继续。');
    expect(screen.queryByText('unauthorized')).not.toBeInTheDocument();
    expect(sessionStorage.getItem('torrentfs.access-token')).toBeNull();
    // The refresh is the first request after the initial dashboard queries.
    expect(authorization(fetchMock.mock.calls[5][1])).toBe('Bearer opaque');
  });

  it('does not show a session notice after intentional logout and allows signing in again', async () => {
    let loginCount = 0;
    const fetchMock = mockFetch((url, request) => {
      if (url.endsWith('/auth/login')) {
        loginCount += 1;
        return loginResponse(loginCount === 1 ? 'opaque' : 'opaque-new');
      }
      if (url.endsWith('/auth/logout')) {
        return new Response(null, { status: 204 });
      }
      return dashboardHandler({
        list: (listRequest) => (authorization(listRequest) === null ? unauthorizedResponse() : jsonResponse([])),
      })(url, request);
    });
    renderApp();

    await waitFor(() => expect(screen.getByRole('heading', { name: '登录 TorrentFS' })).toBeInTheDocument());
    await submitLogin();
    await waitFor(() => expect(screen.getByRole('heading', { name: '任务列表' })).toBeInTheDocument());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(5));

    fireEvent.click(screen.getByRole('button', { name: '退出登录' }));

    await waitFor(() => expect(screen.getByRole('heading', { name: '登录 TorrentFS' })).toBeInTheDocument());
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
    expect(screen.queryByText('需要登录或当前会话已过期，请重新登录后继续。')).not.toBeInTheDocument();
    expect(sessionStorage.getItem('torrentfs.access-token')).toBeNull();
    expect(authorization(fetchMock.mock.calls[5][1])).toBe('Bearer opaque');
    expect(authorization(fetchMock.mock.calls[6][1])).toBeNull();

    await submitLogin();
    await waitFor(() => expect(screen.getByRole('heading', { name: '任务列表' })).toBeInTheDocument());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(11));
    expect(authorization(fetchMock.mock.calls[7][1])).toBeNull();
    expect(authorization(fetchMock.mock.calls[8][1])).toBe('Bearer opaque-new');
  });

  it('keeps invalid passwords separate from the session notice', async () => {
    const fetchMock = mockFetch(unauthorizedResponse);
    renderApp();

    await waitFor(() => expect(screen.getByRole('heading', { name: '登录 TorrentFS' })).toBeInTheDocument());
    await submitLogin('wrong');

    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('密码错误。'));
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(authorization(fetchMock.mock.calls[1][1])).toBeNull();
  });
});
