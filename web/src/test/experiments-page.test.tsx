import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { ExperimentsPage } from '../pages/ExperimentsPage';
import { theme } from '../styles/theme';
import type { LabView } from '../types/api';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

function labView(overrides: Partial<LabView> = {}): LabView {
  return {
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
          <MemoryRouter>
            <ExperimentsPage />
          </MemoryRouter>
        </AuthContext.Provider>
      </MantineProvider>
    </QueryClientProvider>,
  );
  return queryClient;
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => cleanup());

describe('ExperimentsPage', () => {
  it('renders the running experiment and the live counters', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(labView())));
    renderPage(fetchMock);

    await waitFor(() => expect(screen.getByRole('heading', { name: '上传实验监控' })).toBeInTheDocument());
    await waitFor(() => expect(screen.getAllByText('注入虚报').length).toBeGreaterThan(0));
    expect(screen.getByText('实验模式')).toBeInTheDocument();
    expect(screen.getByText('虚报速率')).toBeInTheDocument();
    expect(screen.getByText('2MiB/s')).toBeInTheDocument();
    expect(screen.getByText('42')).toBeInTheDocument();
    expect(screen.getByText('/var/log/torrentfs/lab.jsonl')).toBeInTheDocument();
    expect(screen.getByText('5 秒')).toBeInTheDocument();
    expect(screen.getByText('实时真实上传')).toBeInTheDocument();
    expect(screen.getByText('Tracker 上报')).toBeInTheDocument();
    expect(screen.getByText('+4.0 MiB')).toBeInTheDocument();
  });

  it('does not offer a settings form: the configuration page owns the values', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(labView())));
    renderPage(fetchMock);

    await waitFor(() => expect(screen.getByRole('heading', { name: '上传实验监控' })).toBeInTheDocument());
    expect(screen.queryByRole('form')).not.toBeInTheDocument();
    expect(screen.queryByRole('combobox')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /保存/ })).not.toBeInTheDocument();
    const configLink = screen.getByRole('link', { name: '配置页面' });
    expect(configLink).toHaveAttribute('href', '/config');
    expect(fetchMock).not.toHaveBeenCalledWith(expect.stringContaining('/experiments/upload/settings'), expect.anything());
  });

  it('shows the baseline default without hiding the page', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(labView({
      settings: { mode: 'baseline', rate_bytes_per_second: 0, multiplier: 0, jitter_percent: 20, cap_bytes: 0 },
      seed: 7,
      announce_log: '',
      peer_sample_interval_seconds: 5,
      totals: {
        real_uploaded_bytes: 0,
        real_uploaded_at_last_announce_bytes: 0,
        reported_uploaded_bytes: 0,
        overreported_bytes: 0,
        pending_real_upload_bytes: 0,
      },
    }))));
    renderPage(fetchMock);

    await waitFor(() => expect(screen.getByRole('heading', { name: '上传实验监控' })).toBeInTheDocument());
    await waitFor(() => expect(screen.getAllByText('基线（真实上报）').length).toBeGreaterThan(0));
    expect(screen.getByText('未开启')).toBeInTheDocument();
  });

  it('offers a retry when the first read fails', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse({ error: 'boom' }, 400))
      .mockResolvedValue(jsonResponse(labView()));
    renderPage(fetchMock);

    await waitFor(() => expect(screen.getByText('无法读取上传实验状态')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: '重新读取' }));
    await waitFor(() => expect(screen.getByRole('heading', { name: '上传实验监控' })).toBeInTheDocument());
    await waitFor(() => expect(screen.getAllByText('注入虚报').length).toBeGreaterThan(0));
  });
});
