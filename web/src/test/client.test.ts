import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { ApiClient } from '../api/client';
import { ApiError } from '../api/errors';

beforeEach(() => vi.stubGlobal('fetch', vi.fn()));
afterEach(() => vi.unstubAllGlobals());

function response(body: unknown, init: ResponseInit = {}) {
  return new Response(body === undefined ? null : JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' }, ...init });
}

describe('ApiClient', () => {
  it('sends login JSON without a bearer token and accepts a JSON response', async () => {
    vi.mocked(fetch).mockResolvedValue(response({ token: 'opaque', token_type: 'Bearer', expires_in: 60 }));
    const api = new ApiClient({ getToken: () => 'must-not-send' });
    await expect(api.login('alice', 'secret')).resolves.toMatchObject({ token: 'opaque' });
    const request = vi.mocked(fetch).mock.calls[0][1] as RequestInit;
    expect(new Headers(request.headers).get('Authorization')).toBeNull();
    expect(new Headers(request.headers).get('Content-Type')).toBe('application/json');
    expect(request.cache).toBe('no-store');
  });

  it('adds the bearer header to management calls', async () => {
    vi.mocked(fetch).mockResolvedValue(response([]));
    const api = new ApiClient({ getToken: () => 'opaque' });
    await api.listTorrents();
    const request = vi.mocked(fetch).mock.calls[0][1] as RequestInit;
    expect(new Headers(request.headers).get('Authorization')).toBe('Bearer opaque');
  });

  it('lets fetch set multipart boundaries', async () => {
    vi.mocked(fetch).mockResolvedValue(response({ id: 'a' }));
    const api = new ApiClient({ getToken: () => 'opaque' });
    await api.addTorrent(new File(['payload'], 'x.torrent'));
    const request = vi.mocked(fetch).mock.calls[0][1] as RequestInit;
    expect(new Headers(request.headers).get('Content-Type')).toBeNull();
    expect(request.body).toBeInstanceOf(FormData);
  });

  it('reports WWW-Authenticate and invokes the unauthorized hook only for management calls', async () => {
    const onUnauthorized = vi.fn();
    vi.mocked(fetch)
      .mockResolvedValueOnce(response({ error: 'unauthorized' }, { status: 401, statusText: 'Unauthorized', headers: { 'WWW-Authenticate': 'Bearer' } }))
      .mockResolvedValueOnce(response({ error: 'invalid credentials' }, { status: 401, statusText: 'Unauthorized' }));
    const api = new ApiClient({ getToken: () => 'opaque', onUnauthorized });
    await expect(api.listTorrents()).rejects.toMatchObject({ status: 401, wwwAuthenticate: 'Bearer' });
    expect(onUnauthorized).toHaveBeenCalledOnce();
    await expect(api.login('alice', 'wrong')).rejects.toBeInstanceOf(ApiError);
    expect(onUnauthorized).toHaveBeenCalledOnce();
  });

  it('accepts a 204 logout with no JSON body', async () => {
    vi.mocked(fetch).mockResolvedValue(new Response(null, { status: 204 }));
    const api = new ApiClient();
    await expect(api.logout('opaque')).resolves.toBeUndefined();
  });
});
