import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { App } from '../app/App';
import { ConfigPage } from '../pages/ConfigPage';
import { theme } from '../styles/theme';
import type { ConfigKey, ConfigView } from '../types/api';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

function configKey(overrides: Partial<ConfigKey> & Pick<ConfigKey, 'path' | 'section'>): ConfigKey {
  return {
    kind: 'string',
    values: [],
    default: '',
    file_value: '',
    effective_value: '',
    source: 'default',
    environment: '',
    overridable: true,
    secret: false,
    component: '',
    hot_apply: false,
    documentation: '',
    read_only_reason: '',
    ...overrides,
  };
}

function configView(overrides: Partial<ConfigView> = {}): ConfigView {
  return {
    config_file: '/etc/torrentfs/torrentfs.toml',
    baseline: 'baseline-digest',
    read_only_reason: '',
    keys: [
      configKey({
        path: 'backend',
        section: '',
        kind: 'enum',
        values: ['none', 'fuse', 'smb'],
        default: 'none',
        file_value: 'smb',
        effective_value: 'smb',
        source: 'file',
        component: 'process',
        documentation: 'Backend selects the output backend: none, fuse, or smb.',
      }),
      configKey({
        path: 'log.level',
        section: 'log',
        default: 'info',
        file_value: 'info',
        effective_value: 'debug',
        source: 'environment',
        environment: 'TORRENTFS_LOG_LEVEL',
        component: 'logging',
        hot_apply: true,
        documentation: 'Level is the minimum level of a log record: debug, info, warn, or error.',
        read_only_reason: 'environment_override',
      }),
      configKey({
        path: 'cache.capacity',
        section: 'cache',
        kind: 'byte_quantity',
        default: '2GiB',
        file_value: '2GiB',
        effective_value: '2GiB',
        source: 'default',
        component: 'cache',
        documentation: 'CapacityBytes is the maximum size of the in-memory piece cache.',
      }),
      configKey({
        path: 'experiments.upload.mode',
        section: 'experiments.upload',
        kind: 'enum',
        values: ['baseline', 'inject', 'amplify'],
        default: 'baseline',
        file_value: 'baseline',
        effective_value: 'inject',
        source: 'file',
        component: 'experiment',
        documentation: 'Mode selects the reporting model: baseline, inject, or amplify.',
      }),
      configKey({
        path: 'password',
        section: '',
        secret: true,
        read_only_reason: 'secret',
        environment: 'TORRENTFS_PASSWORD',
        component: 'auth',
        documentation: 'Password is the plaintext password shared by the SMB output and the HTTP login.',
      }),
    ],
    last_save: { at: null, ok: false, error: '', keys: [] },
    last_reload: { at: null, ok: false, error: '', keys: [] },
    ...overrides,
  };
}

function authValue(api: ApiClient): AuthContextValue {
  return {
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
}

function renderPage(fetchMock: ReturnType<typeof vi.fn>, view: ConfigView = configView()) {
  const api = new ApiClient();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  vi.stubGlobal('fetch', fetchMock);
  render(
    <QueryClientProvider client={queryClient}>
      <MantineProvider theme={theme} defaultColorScheme="light">
        <AuthContext.Provider value={authValue(api)}>
          <MemoryRouter initialEntries={['/config']}>
            <ConfigPage />
          </MemoryRouter>
        </AuthContext.Provider>
      </MantineProvider>
    </QueryClientProvider>,
  );
  return view;
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('ConfigPage', () => {
  it('renders every key grouped by section with values, source, documentation and apply badges', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(configView())));
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'log.level' });
    expect(screen.getByText('基础')).toBeInTheDocument();
    expect(screen.getByText('日志')).toBeInTheDocument();
    expect(screen.getByText('缓存')).toBeInTheDocument();
    expect(screen.getByText('上传实验')).toBeInTheDocument();

    const level = screen.getByRole('article', { name: 'log.level' });
    expect(within(level).getByTestId('config-effective-value')).toHaveTextContent('debug');
    expect(within(level).getByTestId('config-file-value')).toHaveTextContent('info');
    expect(within(level).getByTestId('config-default-value')).toHaveTextContent('info');
    expect(within(level).getByTestId('config-source')).toHaveTextContent('环境变量');
    expect(within(level).getByText('立即生效')).toBeInTheDocument();
    expect(within(level).getByText(/minimum level of a log record/)).toBeInTheDocument();

    const capacity = screen.getByRole('article', { name: 'cache.capacity' });
    expect(within(capacity).getByTestId('config-effective-value')).toHaveTextContent('2GiB');
    expect(within(capacity).getByTestId('config-source')).toHaveTextContent('内置默认');
    expect(within(capacity).getByText('重启生效')).toBeInTheDocument();

    const backend = screen.getByRole('article', { name: 'backend' });
    expect(within(backend).getByTestId('config-source')).toHaveTextContent('配置文件');
    expect(within(backend).getByTestId('config-default-value')).toHaveTextContent('none');

    const [url, request] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe('/api/v1/config');
    expect(request.method ?? 'GET').toBe('GET');

    // The page edits the file layer: an editable key carries a control, an
    // environment-overridden key and a secret do not.
    expect(screen.getByRole('textbox', { name: 'cache.capacity' })).toHaveValue('2GiB');
    expect(screen.getByRole('textbox', { name: 'log.level' })).toBeDisabled();
    expect(screen.queryByRole('textbox', { name: 'password' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: '保存' })).toBeDisabled();
  });

  it('renders the upload rate section from the schema without a hand-written form', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(configView({
      keys: [
        configKey({
          path: 'upload_rate.limit',
          section: 'upload_rate',
          kind: 'byte_rate',
          default: '',
          file_value: '1MiB/s',
          effective_value: '1MiB/s',
          source: 'file',
          component: 'session',
          hot_apply: true,
          documentation: 'Limit is the aggregate payload upload rate across every torrent and peer.',
        }),
        configKey({
          path: 'upload_rate.schedule_start',
          section: 'upload_rate',
          default: '',
          file_value: '08:00',
          effective_value: '08:00',
          source: 'file',
          component: 'session',
          hot_apply: true,
          documentation: 'ScheduleStart is the start of the daily window in strict 24-hour HH:MM.',
        }),
      ],
    }))));
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'upload_rate.limit' });
    expect(screen.getByText('上传限速')).toBeInTheDocument();
    const limit = screen.getByRole('article', { name: 'upload_rate.limit' });
    expect(within(limit).getByTestId('config-effective-value')).toHaveTextContent('1MiB/s');
    expect(within(limit).getByText('立即生效')).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'upload_rate.limit' })).toHaveValue('1MiB/s');
    expect(screen.getByRole('textbox', { name: 'upload_rate.schedule_start' })).toHaveValue('08:00');
  });

  it('filters the keys by a search term across paths, documentation and sections', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(configView())));
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'log.level' });
    fireEvent.change(screen.getByRole('textbox', { name: '搜索配置项' }), { target: { value: '缓存' } });

    expect(screen.getByRole('article', { name: 'cache.capacity' })).toBeInTheDocument();
    expect(screen.queryByRole('article', { name: 'log.level' })).not.toBeInTheDocument();
    expect(screen.queryByRole('article', { name: 'backend' })).not.toBeInTheDocument();

    fireEvent.change(screen.getByRole('textbox', { name: '搜索配置项' }), { target: { value: 'minimum level' } });
    expect(screen.getByRole('article', { name: 'log.level' })).toBeInTheDocument();
    expect(screen.queryByRole('article', { name: 'cache.capacity' })).not.toBeInTheDocument();

    fireEvent.change(screen.getByRole('textbox', { name: '搜索配置项' }), { target: { value: '没有这个配置项' } });
    expect(screen.getByText('没有匹配的配置项')).toBeInTheDocument();
  });

  it('explains the environment override and the secret key without disclosing the password', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(configView())));
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'log.level' });

    const level = screen.getByRole('article', { name: 'log.level' });
    expect(within(level).getByText('环境变量覆盖')).toBeInTheDocument();
    expect(within(level).getByText(/TORRENTFS_LOG_LEVEL/)).toBeInTheDocument();
    expect(within(level).getByText(/配置文件中的值不会生效/)).toBeInTheDocument();

    const password = screen.getByRole('article', { name: 'password' });
    expect(within(password).getByText('口令只读')).toBeInTheDocument();
    expect(within(password).getByTestId('config-effective-value')).toHaveTextContent('••••••');
    expect(within(password).getByText(/不会在页面上显示/)).toBeInTheDocument();
    expect(screen.queryByText('s3cr3t')).not.toBeInTheDocument();
  });

  it('explains the not-configured read-only state', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(configView({
      config_file: '',
      read_only_reason: 'not_configured',
    }))));
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'log.level' });
    expect(screen.getByText('配置页面只读：没有加载配置文件')).toBeInTheDocument();
    expect(screen.getByText(/-config/)).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'cache.capacity' })).toBeDisabled();
    expect(screen.queryByRole('button', { name: /保存/ })).not.toBeInTheDocument();
  });

  it('explains the not-writable read-only state', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(configView({
      read_only_reason: 'file_not_writable',
    }))));
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'log.level' });
    expect(screen.getByText(/配置文件不可写/)).toBeInTheDocument();
    expect(screen.getByText(/写权限/)).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'cache.capacity' })).toBeDisabled();
    expect(screen.queryByRole('button', { name: /保存/ })).not.toBeInTheDocument();
  });

  it('is reachable from the application route and exposes a navigation entry', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(configView())));
    vi.stubGlobal('fetch', fetchMock);
    const api = new ApiClient();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    render(
      <QueryClientProvider client={queryClient}>
        <MantineProvider theme={theme} defaultColorScheme="light">
          <AuthContext.Provider value={authValue(api)}>
            <MemoryRouter initialEntries={['/config']}>
              <App />
            </MemoryRouter>
          </AuthContext.Provider>
        </MantineProvider>
      </QueryClientProvider>,
    );

    await screen.findByRole('article', { name: 'log.level' });
    expect(screen.getByRole('link', { name: '配置' })).toHaveAttribute('href', '/config');
  });
});

describe('ConfigPage save flow', () => {
  it('saves a sparse patch with the baseline and groups the applied and restart keys', async () => {
    const view = configView();
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/v1/config' && init?.method === 'PUT') {
        return Promise.resolve(jsonResponse({
          applied: [],
          restart_required: ['cache.capacity'],
          persisted: true,
        }));
      }
      return Promise.resolve(jsonResponse(view));
    });
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'cache.capacity' });
    expect(screen.getByRole('button', { name: '保存' })).toBeDisabled();
    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '1GiB' } });
    expect(screen.getByTestId('config-dirty-count')).toHaveTextContent('未保存 1 项');

    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await screen.findByText('保存成功');
    expect(screen.getByTestId('config-save-summary')).toHaveTextContent('已生效 0 项，重启后生效 1 项');
    expect(screen.getByTestId('config-restart-keys')).toHaveTextContent('cache.capacity');

    const putCall = fetchMock.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === 'PUT');
    expect(putCall).toBeDefined();
    const request = putCall?.[1] as RequestInit;
    expect(JSON.parse(request.body as string)).toEqual({
      baseline: 'baseline-digest',
      patch: { 'cache.capacity': '1GiB' },
    });
  });

  it('shows a per-key validation error and keeps the draft', async () => {
    const view = configView();
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/v1/config' && init?.method === 'PUT') {
        return Promise.resolve(jsonResponse({
          error: '配置校验失败',
          code: 'invalid',
          field_errors: [{ path: 'cache.capacity', message: 'config: cache.capacity: must be between 1 and the cache watermark safety limit' }],
          applied: [],
          restart_required: [],
          persisted: false,
        }, 400));
      }
      return Promise.resolve(jsonResponse(view));
    });
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'cache.capacity' });
    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '0B' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await screen.findByText('保存未完成');
    const capacity = screen.getByRole('article', { name: 'cache.capacity' });
    expect(within(capacity).getByRole('alert')).toHaveTextContent('must be between 1');
    expect(screen.getByRole('textbox', { name: 'cache.capacity' })).toHaveValue('0B');
    expect(screen.getByText(/配置校验失败/)).toBeInTheDocument();
  });

  it('reports a durability failure as applied but unconfirmed', async () => {
    const view = configView();
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/v1/config' && init?.method === 'PUT') {
        return Promise.resolve(jsonResponse({
          error: '新配置已生效，但无法确认重启后仍能保留',
          code: 'durability_unconfirmed',
          field_errors: [],
          applied: ['cache.capacity'],
          restart_required: [],
          persisted: false,
        }, 503));
      }
      return Promise.resolve(jsonResponse(view));
    });
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'cache.capacity' });
    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '1GiB' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await screen.findByText('新配置已生效，但持久化未确认');
    expect(screen.getByTestId('config-save-summary')).toHaveTextContent('已生效 1 项');
    expect(screen.getByTestId('config-applied-keys')).toHaveTextContent('cache.capacity');
    expect(screen.getByText(/无法确认重启后仍能保留/)).toBeInTheDocument();
  });

  it('reports a conflict without silently rebasing the draft', async () => {
    const view = configView();
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/v1/config' && init?.method === 'PUT') {
        return Promise.resolve(jsonResponse({
          error: '配置文件已被外部修改',
          code: 'conflict',
          field_errors: [],
          applied: [],
          restart_required: [],
          persisted: false,
        }, 409));
      }
      return Promise.resolve(jsonResponse(view));
    });
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'cache.capacity' });
    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '1GiB' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await screen.findByText('保存未完成');
    expect(screen.getByText(/配置文件已被外部修改/)).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'cache.capacity' })).toHaveValue('1GiB');
    const puts = fetchMock.mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method === 'PUT');
    expect(puts).toHaveLength(1);
    const configGets = fetchMock.mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method !== 'PUT');
    expect(configGets).toHaveLength(1);
  });
});
