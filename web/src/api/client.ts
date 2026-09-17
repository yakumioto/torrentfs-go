import type { LoginResponse, Operation, Torrent, TorrentStatus } from '../types/api';
import { apiErrorFromResponse } from './errors';

export interface ApiClientOptions {
  baseUrl?: string;
  getToken?: () => string | undefined;
  onUnauthorized?: () => void;
}

interface RequestOptions {
  auth?: boolean;
  notifyUnauthorized?: boolean;
}

export class ApiClient {
  private readonly baseUrl: string;
  private readonly getToken: () => string | undefined;
  private readonly onUnauthorized?: () => void;

  constructor(options: ApiClientOptions = {}) {
    this.baseUrl = options.baseUrl ?? '/api/v1';
    this.getToken = options.getToken ?? (() => undefined);
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

  deleteTorrent(id: string, purgeData: boolean, signal?: AbortSignal): Promise<Operation> {
    const query = purgeData ? '?purge_data=true' : '';
    return this.request<Operation>(`/torrents/${encodeURIComponent(id)}${query}`, {
      method: 'DELETE',
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
    if (auth) {
      const token = this.getToken();
      if (token !== undefined) {
        headers.set('Authorization', `Bearer ${token}`);
      }
    }

    const response = await fetch(`${this.baseUrl}${path}`, { ...requestInit, headers });
    if (!response.ok) {
      const error = await apiErrorFromResponse(response);
      if (error.unauthorized && notifyUnauthorized) {
        this.onUnauthorized?.();
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
