import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { useAuth } from '../app/auth-context';
import { AuthProvider } from '../app/auth';
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest';

function jsonResponse(body: unknown, init: ResponseInit = {}) {
  return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' }, ...init });
}

function AuthProbe() {
  const auth = useAuth();
  return (
    <div>
      <span data-testid="phase">{auth.phase}</span>
      <button onClick={() => void auth.login('alice', 'secret')}>login</button>
      <button onClick={() => void auth.api.listTorrents().catch(() => undefined)}>request</button>
    </div>
  );
}

function renderAuth() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <AuthProvider><AuthProbe /></AuthProvider>
    </QueryClientProvider>,
  );
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

describe('AuthProvider', () => {
  it('uses the first anonymous list response as the connection probe', async () => {
    vi.mocked(fetch).mockResolvedValueOnce(jsonResponse([]));
    renderAuth();
    await waitFor(() => expect(screen.getByTestId('phase')).toHaveTextContent('anonymous'));
    expect(vi.mocked(fetch)).toHaveBeenCalledOnce();
    const request = vi.mocked(fetch).mock.calls[0][1] as RequestInit;
    expect(new Headers(request.headers).get('Authorization')).toBeNull();
  });

  it('restores a token from sessionStorage before probing', async () => {
    sessionStorage.setItem('torrentfs.access-token', 'restored-token');
    vi.mocked(fetch).mockResolvedValueOnce(jsonResponse([]));
    renderAuth();
    await waitFor(() => expect(screen.getByTestId('phase')).toHaveTextContent('authenticated'));
    const probeRequest = vi.mocked(fetch).mock.calls[0][1] as RequestInit;
    expect(new Headers(probeRequest.headers).get('Authorization')).toBe('Bearer restored-token');
  });

  it('moves to login on an API 401 and keeps login free of bearer headers', async () => {
    vi.mocked(fetch)
      .mockResolvedValueOnce(jsonResponse({ error: 'unauthorized' }, { status: 401, headers: { 'WWW-Authenticate': 'Bearer' } }))
      .mockResolvedValueOnce(jsonResponse({ token: 'opaque', token_type: 'Bearer', expires_in: 60 }));
    renderAuth();
    await waitFor(() => expect(screen.getByTestId('phase')).toHaveTextContent('login'));
    fireEvent.click(screen.getByRole('button', { name: 'login' }));
    await waitFor(() => expect(screen.getByTestId('phase')).toHaveTextContent('authenticated'));
    const loginRequest = vi.mocked(fetch).mock.calls[1][1] as RequestInit;
    expect(new Headers(loginRequest.headers).get('Authorization')).toBeNull();
    expect(loginRequest.cache).toBe('no-store');
  });

  it('returns to login when an authenticated management request is rejected', async () => {
    vi.mocked(fetch)
      .mockResolvedValueOnce(jsonResponse({ error: 'unauthorized' }, { status: 401, headers: { 'WWW-Authenticate': 'Bearer' } }))
      .mockResolvedValueOnce(jsonResponse({ token: 'opaque', token_type: 'Bearer', expires_in: 60 }))
      .mockResolvedValueOnce(jsonResponse({ error: 'unauthorized' }, { status: 401, headers: { 'WWW-Authenticate': 'Bearer' } }));
    renderAuth();
    await waitFor(() => expect(screen.getByTestId('phase')).toHaveTextContent('login'));
    fireEvent.click(screen.getByRole('button', { name: 'login' }));
    await waitFor(() => expect(screen.getByTestId('phase')).toHaveTextContent('authenticated'));
    fireEvent.click(screen.getByRole('button', { name: 'request' }));
    await waitFor(() => expect(screen.getByTestId('phase')).toHaveTextContent('login'));
    const managementRequest = vi.mocked(fetch).mock.calls[2][1] as RequestInit;
    expect(new Headers(managementRequest.headers).get('Authorization')).toBe('Bearer opaque');
  });

  it('ignores a stale unauthorized response after a new login', async () => {
    let resolveStale!: (response: Response) => void;
    const staleRequest = new Promise<Response>((resolve) => {
      resolveStale = resolve;
    });
    vi.mocked(fetch)
      .mockResolvedValueOnce(jsonResponse({ error: 'unauthorized' }, { status: 401, headers: { 'WWW-Authenticate': 'Bearer' } }))
      .mockResolvedValueOnce(jsonResponse({ token: 'opaque', token_type: 'Bearer', expires_in: 60 }))
      .mockImplementationOnce(() => staleRequest)
      .mockResolvedValueOnce(jsonResponse({ token: 'opaque', token_type: 'Bearer', expires_in: 60 }))
      .mockResolvedValueOnce(jsonResponse([]));
    renderAuth();

    await waitFor(() => expect(screen.getByTestId('phase')).toHaveTextContent('login'));
    fireEvent.click(screen.getByRole('button', { name: 'login' }));
    await waitFor(() => expect(screen.getByTestId('phase')).toHaveTextContent('authenticated'));

    fireEvent.click(screen.getByRole('button', { name: 'request' }));
    fireEvent.click(screen.getByRole('button', { name: 'login' }));
    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalledTimes(4));
    resolveStale(jsonResponse({ error: 'unauthorized' }, { status: 401, headers: { 'WWW-Authenticate': 'Bearer' } }));
    await waitFor(() => expect(screen.getByTestId('phase')).toHaveTextContent('authenticated'));

    fireEvent.click(screen.getByRole('button', { name: 'request' }));
    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalledTimes(5));
    const latestRequest = vi.mocked(fetch).mock.calls[4][1] as RequestInit;
    expect(new Headers(latestRequest.headers).get('Authorization')).toBe('Bearer opaque');
  });
});
