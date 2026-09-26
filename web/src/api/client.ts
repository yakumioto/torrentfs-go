import type { LoginResponse, Operation, PruneResult, RuntimeStats, Torrent, TorrentStatus } from '../types/api';
import { apiErrorFromResponse } from './errors';

export interface ApiClientOptions {
  baseUrl?: string;
  getToken?: () => string | undefined;
  getAuthRevision?: () => number;
  onUnauthorized?: (requestToken?: string, requestSignal?: AbortSignal | null, requestRevision?: number) => void;
}

interface RequestOptions {
  auth?: boolean;
  notifyUnauthorized?: boolean;
}

export class ApiClient {
  private readonly baseUrl: string;
  private readonly getToken: () => string | undefined;
  private readonly getAuthRevision?: () => number;
  private readonly onUnauthorized?: (requestToken?: string, requestSignal?: AbortSignal | null, requestRevision?: number) => void;

  constructor(options: ApiClientOptions = {}) {
    this.baseUrl = options.baseUrl ?? '/api/v1';
    this.getToken = options.getToken ?? (() => undefined);
    this.getAuthRevision = options.getAuthRevision;
    this.onUnauthorized = options.onUnauthorized;
  }

  async login(username: string, password: string, signal?: AbortSignal): Promise<LoginResponse> {
    return this.request<LoginResponse>('/auth/login', {
      method: 'POST',
      body: JSON.stringify({ username, password }),
      cache: 'no-store',
      signal,
    }, { notifyUnauthorized: false, auth: false });
  }

  async logout(token: string, signal?: AbortSignal): Promise<void> {
    const headers = new Headers({ Authorization: `Bearer ${token}` });
    await this.request<void>('/auth/logout', {
      method: 'POST',
      headers,
      cache: 'no-store',
      signal,
    }, { notifyUnauthorized: false, auth: false });
  }

  listTorrents(signal?: AbortSignal): Promise<Torrent[]> {
    return this.request<Torrent[]>('/torrents', { signal });
  }

  getRuntimeStats(signal?: AbortSignal): Promise<RuntimeStats> {
    return this.request<RuntimeStats>('/stats', { signal });
  }

  getTorrent(id: string, signal?: AbortSignal): Promise<Torrent> {
    return this.request<Torrent>(`/torrents/${encodeURIComponent(id)}`, { signal });
  }

  getTorrentStatus(id: string, signal?: AbortSignal): Promise<TorrentStatus> {
    return this.request<TorrentStatus>(`/torrents/${encodeURIComponent(id)}/status`, { signal });
  }

  addMagnet(magnetUri: string, signal?: AbortSignal): Promise<Torrent> {
    return this.request<Torrent>('/torrents', {
      method: 'POST',
      body: JSON.stringify({ magnet_uri: magnetUri }),
      signal,
    });
  }

  addTorrent(file: File, signal?: AbortSignal): Promise<Torrent> {
    const body = new FormData();
    body.append('file', file);
    return this.request<Torrent>('/torrents', { method: 'POST', body, signal });
  }

  deleteTorrent(id: string, signal?: AbortSignal): Promise<Operation> {
    return this.request<Operation>(`/torrents/${encodeURIComponent(id)}`, {
      method: 'DELETE',
      signal,
    });
  }

  setFavorite(id: string, favorite: boolean, signal?: AbortSignal): Promise<Torrent> {
    return this.request<Torrent>(`/torrents/${encodeURIComponent(id)}/favorite`, {
      method: 'PUT',
      body: JSON.stringify({ favorite }),
      signal,
    });
  }

  pruneTorrents(olderThanDays: number, signal?: AbortSignal): Promise<PruneResult> {
    return this.request<PruneResult>('/torrents/prune', {
      method: 'POST',
      body: JSON.stringify({ older_than_days: olderThanDays }),
      signal,
    });
  }

  getOperation(operationId: string, signal?: AbortSignal): Promise<Operation> {
    return this.request<Operation>(`/operations/${encodeURIComponent(operationId)}`, { signal });
  }

  private async request<T>(path: string, init: RequestInit = {}, options: RequestOptions = {}): Promise<T> {
    const { notifyUnauthorized = true, auth = true } = options;
    const requestInit = init;
    const headers = new Headers(requestInit.headers);
    headers.set('Accept', 'application/json');
    if (typeof requestInit.body === 'string' && !headers.has('Content-Type')) {
      headers.set('Content-Type', 'application/json');
    }
    let requestToken: string | undefined;
    const requestRevision = auth ? this.getAuthRevision?.() : undefined;
    if (auth) {
      requestToken = this.getToken();
      if (requestToken !== undefined) {
        headers.set('Authorization', `Bearer ${requestToken}`);
      }
    }

    const response = await fetch(`${this.baseUrl}${path}`, { ...requestInit, headers });
    if (!response.ok) {
      const error = await apiErrorFromResponse(response);
      if (error.unauthorized && notifyUnauthorized) {
        if (requestInit.signal !== undefined || requestRevision !== undefined) {
          this.onUnauthorized?.(requestToken, requestInit.signal, requestRevision);
        } else {
          this.onUnauthorized?.(requestToken);
        }
      }
      throw error;
    }
    if (response.status === 204) {
      return undefined as T;
    }

    const body = await response.text();
    if (body === '') {
      return undefined as T;
    }
    return JSON.parse(body) as T;
  }
}
