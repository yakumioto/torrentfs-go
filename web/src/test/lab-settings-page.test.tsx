import { MantineProvider } from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { LabSettingsPage } from '../pages/LabSettingsPage';
import { theme } from '../styles/theme';
import type { LabView } from '../types/api';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

function enabledLab(overrides: Partial<LabView> = {}): LabView {
  return {
    enabled: true,
    settings: { mode: 'inject', rate_bytes_per_second: 2_097_152, multiplier: 0, jitter_percent: 20, cap_bytes: 0 },
    seed: 42,
    announce_log: '/var/log/torrentfs/lab.jsonl',
    peer_sample_interval_seconds: 5,
    totals: {
      real_uploaded_bytes: 5_000_000,
      real_uploaded_at_last_announce_bytes: 0,
      reported_uploaded_bytes: 4_197_247,
      overreported_bytes: 4_197_247,
      pending_real_upload_bytes: 5_000_000,
    },
    torrents: [],
    ...overrides,
  };
}

function disabledLab(): LabView {
  return {
    enabled: false,
    settings: null,
    seed: 0,
    announce_log: '',
    peer_sample_interval_seconds: 0,
    totals: {
      real_uploaded_bytes: 0,
      real_uploaded_at_last_announce_bytes: 0,
      reported_uploaded_bytes: 0,
      overreported_bytes: 0,
      pending_real_upload_bytes: 0,
    },
    torrents: [],
  };
}

function renderPage(fetchMock: ReturnType<typeof vi.fn>) {
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
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } } });
  vi.stubGlobal('fetch', fetchMock);
  render(
    <QueryClientProvider client={queryClient}>
      <MantineProvider theme={theme} defaultColorScheme="light">
        <AuthContext.Provider value={auth}>
          <LabSettingsPage />
        </AuthContext.Provider>
      </MantineProvider>
    </QueryClientProvider>,
  );
  return queryClient;
}

function getLabRequest(fetchMock: ReturnType<typeof vi.fn>, method: 'GET' | 'PUT'): RequestInit | undefined {
  const call = fetchMock.mock.calls.find(([, init]) => ((init as RequestInit | undefined)?.method ?? 'GET') === method);
  return call?.[1] as RequestInit | undefined;
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('LabSettingsPage', () => {
  it('prefills the form from the daemon view', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(enabledLab())));
    renderPage(fetchMock);

    await waitFor(() => expect(screen.getByRole('heading', { name: '上传实验设置' })).toBeInTheDocument());
    await waitFor(() => expect(screen.getByRole('combobox', { name: '实验模式' })).toHaveValue('inject'));
    expect(screen.getByLabelText('虚报速率')).toHaveValue('2MiB/s');
    expect(screen.getByLabelText('抖动百分比')).toHaveValue('20');
    expect(screen.getByLabelText('上报上限（可选）')).toHaveValue('');
    expect(screen.getByText(/amplify 的上报值在每次 announce 时按真实增量 × 倍率累积/)).toBeInTheDocument();

    const [url, request] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe('/api/v1/experiments/upload');
    expect(request.method ?? 'GET').toBe('GET');
  });

  it('saves inject settings as bytes per second and notifies the user', async () => {
    const notification = vi.spyOn(notifications, 'show').mockImplementation(() => 'notification-id');
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === 'PUT') {
        return Promise.resolve(jsonResponse(enabledLab({
          settings: { mode: 'inject', rate_bytes_per_second: 32_000_000, multiplier: 0, jitter_percent: 10, cap_bytes: 0 },
        })));
      }
      return Promise.resolve(jsonResponse(enabledLab()));
    });
    renderPage(fetchMock);

    await waitFor(() => expect(screen.getByLabelText('虚报速率')).toHaveValue('2MiB/s'));
    fireEvent.change(screen.getByLabelText('虚报速率'), { target: { value: '32MB/s' } });
    fireEvent.change(screen.getByLabelText('抖动百分比'), { target: { value: '10' } });
    fireEvent.click(screen.getByRole('button', { name: '保存设置' }));

    await waitFor(() => expect(notification).toHaveBeenCalledWith(expect.objectContaining({ title: '上传实验设置已保存' })));
    const put = getLabRequest(fetchMock, 'PUT');
    expect(put?.body).toBe(JSON.stringify({
      mode: 'inject',
      rate_bytes_per_second: 32_000_000,
      multiplier: 0,
      jitter_percent: 10,
      cap_bytes: 0,
    }));
  });

  it('switches to amplify mode with a multiplier and a human-readable cap', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === 'PUT') {
        return Promise.resolve(jsonResponse(enabledLab({
          settings: { mode: 'amplify', rate_bytes_per_second: 0, multiplier: 3, jitter_percent: 20, cap_bytes: 10 * 1024 ** 3 },
        })));
      }
      return Promise.resolve(jsonResponse(enabledLab()));
    });
    renderPage(fetchMock);

    await waitFor(() => expect(screen.getByRole('combobox', { name: '实验模式' })).toHaveValue('inject'));
    fireEvent.change(screen.getByRole('combobox', { name: '实验模式' }), { target: { value: 'amplify' } });
    fireEvent.change(screen.getByLabelText('放大倍率'), { target: { value: '3' } });
    fireEvent.change(screen.getByLabelText('上报上限（可选）'), { target: { value: '10GiB' } });
    fireEvent.click(screen.getByRole('button', { name: '保存设置' }));

    await waitFor(() => expect(getLabRequest(fetchMock, 'PUT')).toBeDefined());
    expect(getLabRequest(fetchMock, 'PUT')?.body).toBe(JSON.stringify({
      mode: 'amplify',
      rate_bytes_per_second: 0,
      multiplier: 3,
      jitter_percent: 20,
      cap_bytes: 10 * 1024 ** 3,
    }));
  });

  it('blocks saving an invalid multiplier while the form explains why', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(enabledLab())));
    renderPage(fetchMock);

    await waitFor(() => expect(screen.getByRole('combobox', { name: '实验模式' })).toHaveValue('inject'));
    fireEvent.change(screen.getByRole('combobox', { name: '实验模式' }), { target: { value: 'amplify' } });
    fireEvent.change(screen.getByLabelText('放大倍率'), { target: { value: '1' } });

    expect(screen.getByRole('button', { name: '保存设置' })).toBeDisabled();
    expect(screen.getByText('倍率必须是大于 1 的数值。')).toBeInTheDocument();
    expect(getLabRequest(fetchMock, 'PUT')).toBeUndefined();
  });

  it('shows a disabled form and the environment hint when the experiment is off', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(disabledLab())));
    renderPage(fetchMock);

    await waitFor(() => expect(screen.getByText('上传实验未启用')).toBeInTheDocument());
    expect(screen.getByText(/TORRENTFS_EXPERIMENTS_UPLOAD_MODE/)).toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: '实验模式' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '保存设置' })).toBeDisabled();
  });

  it('reports backend lab errors instead of claiming success', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === 'PUT') {
        return Promise.resolve(jsonResponse(
          { error: '上传实验未启用', code: 'lab_disabled', applied: false },
          409,
        ));
      }
      return Promise.resolve(jsonResponse(enabledLab()));
    });
    renderPage(fetchMock);

    await waitFor(() => expect(screen.getByRole('combobox', { name: '实验模式' })).toHaveValue('inject'));
    fireEvent.click(screen.getByRole('button', { name: '保存设置' }));

    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('TORRENTFS_EXPERIMENTS_UPLOAD_MODE'));
    expect(screen.queryByText(/已保存/)).not.toBeInTheDocument();
  });
});
