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

  it('fetches runtime stats through the additive endpoint', async () => {
    const stats = {
      started_at: '2026-09-23T09:00:00Z',
      cache: { used_bytes: 1, capacity_bytes: 2 },
      transfer: { downloaded_bytes: 3, uploaded_bytes: 4 },
    };
    vi.mocked(fetch).mockResolvedValue(response(stats));
    const api = new ApiClient({ getToken: () => 'opaque' });
    await expect(api.getRuntimeStats()).resolves.toEqual(stats);
    expect(String(vi.mocked(fetch).mock.calls[0][0])).toBe('/api/v1/stats');
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
    expect(onUnauthorized).toHaveBeenCalledWith('opaque');
    await expect(api.login('alice', 'wrong')).rejects.toBeInstanceOf(ApiError);
    expect(onUnauthorized).toHaveBeenCalledOnce();
  });

  it('accepts a 204 logout with no JSON body', async () => {
    vi.mocked(fetch).mockResolvedValue(new Response(null, { status: 204 }));
    const api = new ApiClient();
    await expect(api.logout('opaque')).resolves.toBeUndefined();
  });
});

describe('ApiClient subtitle upload', () => {
  it('sends video_path and the file as multipart without a manual boundary', async () => {
    const responseBody = {
      torrent_id: 'abc',
      video_path: 'Movie.2026.mkv',
      path: 'Movie.2026.srt',
      mount_path: 'Show/Movie.2026.srt',
      format: 'srt',
      size: 4,
      updated_at: '2026-09-26T12:00:00Z',
      replaced: false,
    };
    vi.mocked(fetch).mockResolvedValue(response({ ...responseBody }));
    const api = new ApiClient({ getToken: () => 'opaque' });
    await expect(api.uploadSubtitle('abc/def', 'Movie.2026.mkv', new File(['sub'], 'Movie.2026.srt'))).resolves.toEqual(responseBody);

    const call = vi.mocked(fetch).mock.calls[0];
    expect(String(call[0])).toBe('/api/v1/torrents/abc%2Fdef/subtitles');
    const request = call[1] as RequestInit;
    expect(request.method).toBe('PUT');
    expect(new Headers(request.headers).get('Content-Type')).toBeNull();
    const body = request.body as FormData;
    expect(body.get('video_path')).toBe('Movie.2026.mkv');
    expect((body.get('file') as File).name).toBe('Movie.2026.srt');
  });

  it('exposes the stable error code from a rejected upload', async () => {
    vi.mocked(fetch).mockResolvedValue(
      response({ error: '字幕文件名必须与目标视频的名称严格对应。', code: 'subtitle_name_mismatch' }, { status: 415, statusText: 'Unsupported Media Type' }),
    );
    const api = new ApiClient({ getToken: () => 'opaque' });
    await expect(api.uploadSubtitle('abc', 'Movie.2026.mkv', new File(['x'], 'other.srt'))).rejects.toMatchObject({
      status: 415,
      code: 'subtitle_name_mismatch',
    });
  });
});

describe('ApiClient favorite and prune calls', () => {
  it('sends a favorite update to the per-torrent endpoint', async () => {
    vi.mocked(fetch).mockResolvedValue(response({ id: 'abc', favorite: true }));
    const api = new ApiClient({ getToken: () => 'opaque' });
    await api.setFavorite('abc/def', true);

    const call = vi.mocked(fetch).mock.calls[0];
    expect(String(call[0])).toBe('/api/v1/torrents/abc%2Fdef/favorite');
    const request = call[1] as RequestInit;
    expect(request.method).toBe('PUT');
    expect(request.body).toBe(JSON.stringify({ favorite: true }));
  });

  it('sends the prune threshold as older_than_days', async () => {
    vi.mocked(fetch).mockResolvedValue(response({ operations: [], excluded_favorites: 0 }));
    const api = new ApiClient({ getToken: () => 'opaque' });
    await expect(api.pruneTorrents(30)).resolves.toEqual({ operations: [], excluded_favorites: 0 });

    const call = vi.mocked(fetch).mock.calls[0];
    expect(String(call[0])).toBe('/api/v1/torrents/prune');
    const request = call[1] as RequestInit;
    expect(request.method).toBe('POST');
    expect(request.body).toBe(JSON.stringify({ older_than_days: 30 }));
  });
});
