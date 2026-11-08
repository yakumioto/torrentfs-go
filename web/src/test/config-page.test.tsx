import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { createMemoryRouter, RouterProvider } from 'react-router-dom';
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

/** The full `[experiments.upload]` schema as the configuration view reports it. */
function uploadExperimentKeys(): ConfigKey[] {
  return [
    configKey({
      path: 'experiments.upload.mode',
      section: 'experiments.upload',
      kind: 'enum',
      values: ['baseline', 'inject', 'amplify'],
      default: 'baseline',
      file_value: 'inject',
      effective_value: 'inject',
      source: 'file',
      component: 'experiment',
      hot_apply: true,
      documentation: 'Mode selects the reporting model: baseline, inject, or amplify.',
    }),
    configKey({
      path: 'experiments.upload.rate',
      section: 'experiments.upload',
      kind: 'byte_rate',
      default: '',
      file_value: '1MiB/s',
      effective_value: '1MiB/s',
      source: 'file',
      component: 'experiment',
      hot_apply: true,
      documentation: 'Rate is the average synthetic growth of the reported upload in inject mode.',
    }),
    configKey({
      path: 'experiments.upload.multiplier',
      section: 'experiments.upload',
      kind: 'ratio',
      default: '',
      file_value: '2.5',
      effective_value: '2.5',
      source: 'file',
      component: 'experiment',
      hot_apply: true,
      documentation: 'Multiplier grows the reported upload in amplify mode.',
    }),
    configKey({
      path: 'experiments.upload.jitter',
      section: 'experiments.upload',
      kind: 'percent',
      default: '20',
      file_value: '20',
      effective_value: '20',
      source: 'default',
      component: 'experiment',
      hot_apply: true,
      documentation: 'Jitter is the per-announce deviation in percent.',
    }),
    configKey({
      path: 'experiments.upload.cap',
      section: 'experiments.upload',
      kind: 'byte_quantity',
      default: '',
      file_value: '8MiB',
      effective_value: '8MiB',
      source: 'file',
      component: 'experiment',
      hot_apply: true,
      documentation: 'Cap is the per-torrent ceiling on the cumulative reported upload.',
    }),
    configKey({
      path: 'experiments.upload.seed',
      section: 'experiments.upload',
      kind: 'int',
      default: '0',
      file_value: '42',
      effective_value: '42',
      source: 'file',
      component: 'experiment',
      documentation: 'Seed fixes the random jitter sequence for reproducible runs.',
    }),
    configKey({
      path: 'experiments.upload.announce_log',
      section: 'experiments.upload',
      default: '',
      file_value: '/var/log/torrentfs/announce.jsonl',
      effective_value: '/var/log/torrentfs/announce.jsonl',
      source: 'file',
      component: 'experiment',
      documentation: 'AnnounceLog is the JSON Lines file that receives one record per announce.',
    }),
    configKey({
      path: 'experiments.upload.peer_sample_interval',
      section: 'experiments.upload',
      kind: 'duration',
      default: '',
      file_value: '10s',
      effective_value: '10s',
      source: 'file',
      component: 'experiment',
      documentation: 'PeerSampleInterval is how often peer activity is sampled.',
    }),
  ];
}

function configView(overrides: Partial<ConfigView> = {}): ConfigView {
  return {
    config_file: '/etc/torrentfs/torrentfs.toml',
    baseline: 'baseline-digest',
    read_only_reason: '',
    file_error: '',
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

function disabledExperiment() {
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

function renderPage(
  fetchMock: ReturnType<typeof vi.fn>,
  view: ConfigView = configView(),
  path = '/config',
  entries?: string[],
  initialIndex?: number,
) {
  const api = new ApiClient();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  vi.stubGlobal('fetch', fetchMock);
  // The configuration page guards navigation through the data router blocker,
  // so the tests render it through a data router like the application does.
  const router = createMemoryRouter([{ path: '*', element: <ConfigPage /> }], {
    initialEntries: entries ?? [path],
    initialIndex,
  });
  render(
    <QueryClientProvider client={queryClient}>
      <MantineProvider theme={theme} defaultColorScheme="light">
        <AuthContext.Provider value={authValue(api)}>
          <RouterProvider router={router} />
        </AuthContext.Provider>
      </MantineProvider>
    </QueryClientProvider>,
  );
  return { view, router };
}

function renderAppAt(fetchMock: ReturnType<typeof vi.fn>, path = '/config') {
  const api = new ApiClient();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  vi.stubGlobal('fetch', fetchMock);
  const router = createMemoryRouter([{ path: '*', element: <App /> }], {
    initialEntries: [path],
  });
  render(
    <QueryClientProvider client={queryClient}>
      <MantineProvider theme={theme} defaultColorScheme="light">
        <AuthContext.Provider value={authValue(api)}>
          <RouterProvider router={router} />
        </AuthContext.Provider>
      </MantineProvider>
    </QueryClientProvider>,
  );
  return router;
}

function beforeUnloadEvent(): Event {
  const event = new Event('beforeunload', { cancelable: true });
  window.dispatchEvent(event);
  return event;
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
    // The page never offers a process restart or exit control.
    expect(screen.queryByRole('button', { name: /重启服务|重启进程|退出进程|退出程序/ })).not.toBeInTheDocument();
  });

  it('maps every schema kind to its editor control and saves the edited text', async () => {
    const view = configView({
      keys: [
        configKey({
          path: 'log.add_source',
          section: 'log',
          kind: 'bool',
          default: 'false',
          file_value: 'false',
          effective_value: 'false',
          source: 'file',
          component: 'logging',
          documentation: 'AddSource adds the source file and line to every log record.',
        }),
        configKey({
          path: 'experiments.upload.seed',
          section: 'experiments.upload',
          kind: 'int',
          default: '0',
          file_value: '0',
          effective_value: '0',
          source: 'file',
          component: 'experiment',
          documentation: 'Seed fixes the random jitter sequence for reproducible runs.',
        }),
        configKey({
          path: 'connections.bootstrap_nodes',
          section: 'connections',
          kind: 'list',
          default: '[]',
          file_value: '',
          effective_value: '',
          source: 'default',
          component: 'session',
          documentation: 'BootstrapNodes lists explicit host:port entries.',
        }),
        configKey({
          path: 'filesystem.read_timeout',
          section: 'filesystem',
          kind: 'duration',
          default: '30s',
          file_value: '30s',
          effective_value: '30s',
          source: 'file',
          component: 'filesystem',
          documentation: 'ReadTimeout bounds a single read of a piece.',
        }),
      ],
    });
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/v1/config' && init?.method === 'PUT') {
        return Promise.resolve(jsonResponse({
          applied: [],
          restart_required: [
            'log.add_source',
            'experiments.upload.seed',
            'connections.bootstrap_nodes',
            'filesystem.read_timeout',
          ],
          persisted: true,
        }));
      }
      return Promise.resolve(jsonResponse(view));
    });
    renderPage(fetchMock, view);

    await screen.findByRole('article', { name: 'log.add_source' });
    const addSource = within(screen.getByRole('article', { name: 'log.add_source' }))
      .getByRole('switch', { name: 'log.add_source' });
    expect(addSource).not.toBeChecked();
    fireEvent.click(addSource);

    fireEvent.change(
      within(screen.getByRole('article', { name: 'experiments.upload.seed' }))
        .getByRole('textbox', { name: 'experiments.upload.seed' }),
      { target: { value: '7' } },
    );
    fireEvent.change(
      screen.getByRole('textbox', { name: 'connections.bootstrap_nodes' }),
      { target: { value: 'router.example:6881' } },
    );
    fireEvent.change(
      screen.getByRole('textbox', { name: 'filesystem.read_timeout' }),
      { target: { value: '45s' } },
    );

    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    await screen.findByText('保存成功');
    const put = fetchMock.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === 'PUT');
    expect(put).toBeDefined();
    expect(JSON.parse((put?.[1] as RequestInit).body as string)).toEqual({
      baseline: 'baseline-digest',
      patch: {
        'log.add_source': 'true',
        'experiments.upload.seed': '7',
        'connections.bootstrap_nodes': 'router.example:6881',
        'filesystem.read_timeout': '45s',
      },
    });
  });

  it('labels the byte quantity and byte rate kinds the schema reports', async () => {
    const view = configView({
      keys: [
        configKey({
          path: 'cache.capacity',
          section: 'cache',
          kind: 'byte_quantity',
          default: '2GiB',
          file_value: '2GiB',
          effective_value: '2GiB',
          source: 'file',
          component: 'cache',
          documentation: 'CapacityBytes is the maximum size of the in-memory piece cache.',
        }),
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
      ],
    });
    renderPage(vi.fn(() => Promise.resolve(jsonResponse(view))), view);

    const capacity = await screen.findByRole('article', { name: 'cache.capacity' });
    expect(within(capacity).getByText('字节量')).toBeInTheDocument();
    const limit = screen.getByRole('article', { name: 'upload_rate.limit' });
    expect(within(limit).getByText('速率')).toBeInTheDocument();
    // The canonical schema kinds reach the page: a different spelling would
    // fall back to the raw kind text and render it in English.
    expect(screen.queryByText('byte_quantity')).not.toBeInTheDocument();
    expect(screen.queryByText('byte_rate')).not.toBeInTheDocument();
  });

  it('groups the upload experiment keys under an experiments category with a deep-link anchor', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(configView())));
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'experiments.upload.mode' });
    const category = screen.getByTestId('config-category-experiments');
    expect(category).toHaveAttribute('id', 'experiments');
    expect(within(category).getByText('实验')).toBeInTheDocument();
    expect(within(category).getByText(/上传统计实验常驻运行/)).toBeInTheDocument();
    expect(within(category).getByRole('heading', { name: '上传实验' })).toBeInTheDocument();
    expect(within(category).getByRole('article', { name: 'experiments.upload.mode' })).toBeInTheDocument();
  });

  it('scrolls the experiments category into view when opened through its anchor', async () => {
    const originalScrollIntoView = (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView;
    const scrollIntoView = vi.fn();
    Object.defineProperty(Element.prototype, 'scrollIntoView', {
      configurable: true,
      writable: true,
      value: scrollIntoView,
    });
    try {
      const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(configView())));
      renderPage(fetchMock, configView(), '/config#experiments');

      const category = await screen.findByTestId('config-category-experiments');
      await waitFor(() => expect(scrollIntoView).toHaveBeenCalled());
      expect(scrollIntoView.mock.instances.at(-1)).toBe(category);
    } finally {
      if (originalScrollIntoView === undefined) {
        delete (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView;
      } else {
        Object.defineProperty(Element.prototype, 'scrollIntoView', {
          configurable: true,
          writable: true,
          value: originalScrollIntoView,
        });
      }
    }
  });

  it('lists every upload experiment key and saves hot and restart-required edits', async () => {
    const view = configView({ keys: uploadExperimentKeys() });
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/v1/config' && init?.method === 'PUT') {
        return Promise.resolve(jsonResponse({
          applied: ['experiments.upload.mode', 'experiments.upload.rate'],
          restart_required: ['experiments.upload.seed'],
          persisted: true,
        }));
      }
      return Promise.resolve(jsonResponse(view));
    });
    renderPage(fetchMock, view);

    await screen.findByRole('article', { name: 'experiments.upload.mode' });
    const category = screen.getByTestId('config-category-experiments');
    const paths = [
      'experiments.upload.mode',
      'experiments.upload.rate',
      'experiments.upload.multiplier',
      'experiments.upload.jitter',
      'experiments.upload.cap',
      'experiments.upload.seed',
      'experiments.upload.announce_log',
      'experiments.upload.peer_sample_interval',
    ];
    for (const path of paths) {
      const article = within(category).getByRole('article', { name: path });
      expect(article).toBeInTheDocument();
      // Every experiment key is editable: the schema kind chooses the control.
      const role = path === 'experiments.upload.mode' ? 'combobox' : 'textbox';
      expect(within(article).getByRole(role, { name: path })).toBeEnabled();
    }

    const hot = ['mode', 'rate', 'multiplier', 'jitter', 'cap'];
    for (const key of hot) {
      expect(within(screen.getByRole('article', { name: `experiments.upload.${key}` })).getByText('立即生效')).toBeInTheDocument();
    }
    const restart = ['seed', 'announce_log', 'peer_sample_interval'];
    for (const key of restart) {
      expect(within(screen.getByRole('article', { name: `experiments.upload.${key}` })).getByText('重启生效')).toBeInTheDocument();
    }

    const experimentField = (path: string) => within(screen.getByRole('article', { name: path }));
    fireEvent.change(experimentField('experiments.upload.mode').getByRole('combobox', { name: 'experiments.upload.mode' }), { target: { value: 'amplify' } });
    fireEvent.change(experimentField('experiments.upload.rate').getByRole('textbox', { name: 'experiments.upload.rate' }), { target: { value: '2MiB/s' } });
    fireEvent.change(experimentField('experiments.upload.seed').getByRole('textbox', { name: 'experiments.upload.seed' }), { target: { value: '7' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await screen.findByText('保存成功');
    const put = fetchMock.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === 'PUT');
    expect(put).toBeDefined();
    expect(JSON.parse((put?.[1] as RequestInit).body as string)).toEqual({
      baseline: 'baseline-digest',
      patch: {
        'experiments.upload.mode': 'amplify',
        'experiments.upload.rate': '2MiB/s',
        'experiments.upload.seed': '7',
      },
    });
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
    renderAppAt(fetchMock, '/config');

    await screen.findByRole('article', { name: 'log.level' });
    expect(screen.getByRole('link', { name: '配置' })).toHaveAttribute('href', '/config');
  });
});

describe('ConfigPage change preview', () => {
  it('shows only the changed keys and their new values before saving', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(configView())));
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'cache.capacity' });
    expect(screen.queryByTestId('config-diff-preview')).not.toBeInTheDocument();

    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '1GiB' } });

    const preview = screen.getByTestId('config-diff-preview');
    expect(within(preview).getByText('cache.capacity')).toBeInTheDocument();
    expect(within(preview).getByText('1GiB')).toBeInTheDocument();
    // Unchanged keys and the previous value are deliberately absent.
    expect(within(preview).queryByText('2GiB')).not.toBeInTheDocument();
    expect(within(preview).queryByText('backend')).not.toBeInTheDocument();
    expect(within(preview).queryByText('baseline')).not.toBeInTheDocument();

    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '2GiB' } });
    expect(screen.queryByTestId('config-diff-preview')).not.toBeInTheDocument();
  });
});

describe('ConfigPage client-side value checks', () => {
  function unitView(): ConfigView {
    return configView({
      keys: [
        configKey({
          path: 'cache.capacity',
          section: 'cache',
          kind: 'byte_quantity',
          default: '2GiB',
          file_value: '2GiB',
          effective_value: '2GiB',
          source: 'file',
          component: 'cache',
        }),
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
        }),
        configKey({
          path: 'filesystem.read_timeout',
          section: 'filesystem',
          kind: 'duration',
          default: '30s',
          file_value: '30s',
          effective_value: '30s',
          source: 'file',
          component: 'filesystem',
        }),
      ],
    });
  }

  it('rejects a malformed byte quantity before the save request', async () => {
    const fetchMock = vi.fn((_url: string, init?: RequestInit) => {
      if (init?.method === 'PUT') {
        throw new Error('the page must not save an invalid value');
      }
      return Promise.resolve(jsonResponse(unitView()));
    });
    renderPage(fetchMock, unitView());

    await screen.findByRole('article', { name: 'cache.capacity' });
    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '2QB' } });

    const article = screen.getByRole('article', { name: 'cache.capacity' });
    expect(within(article).getByRole('alert')).toHaveTextContent('仅支持 B、KB、MB');
    expect(screen.getByRole('button', { name: '保存' })).toBeDisabled();
    expect(fetchMock.mock.calls.some(([, init]) => (init as RequestInit | undefined)?.method === 'PUT')).toBe(false);

    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '3GiB' } });
    expect(within(article).queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: '保存' })).toBeEnabled();
  });

  it('rejects a rate without the byte unit and a duration with the wrong syntax', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(unitView())));
    renderPage(fetchMock, unitView());

    await screen.findByRole('article', { name: 'upload_rate.limit' });
    fireEvent.change(screen.getByRole('textbox', { name: 'upload_rate.limit' }), { target: { value: '1MiB' } });
    expect(within(screen.getByRole('article', { name: 'upload_rate.limit' })).getByRole('alert'))
      .toHaveTextContent('请输入带单位的速率');

    fireEvent.change(screen.getByRole('textbox', { name: 'filesystem.read_timeout' }), { target: { value: '45' } });
    expect(within(screen.getByRole('article', { name: 'filesystem.read_timeout' })).getByRole('alert'))
      .toHaveTextContent('请输入合法时长');
    expect(screen.getByRole('button', { name: '保存' })).toBeDisabled();

    fireEvent.change(screen.getByRole('textbox', { name: 'upload_rate.limit' }), { target: { value: '2MiB/s' } });
    fireEvent.change(screen.getByRole('textbox', { name: 'filesystem.read_timeout' }), { target: { value: '1h30m' } });
    expect(screen.getByRole('button', { name: '保存' })).toBeEnabled();
  });

  it('accepts the Go duration syntax the configuration file accepts', async () => {
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/v1/config' && init?.method === 'PUT') {
        return Promise.resolve(jsonResponse({ applied: [], restart_required: ['filesystem.read_timeout'], persisted: true }));
      }
      return Promise.resolve(jsonResponse(unitView()));
    });
    renderPage(fetchMock, unitView());

    await screen.findByRole('article', { name: 'filesystem.read_timeout' });
    fireEvent.change(screen.getByRole('textbox', { name: 'filesystem.read_timeout' }), { target: { value: '1h30m' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await screen.findByText('保存成功');
    const put = fetchMock.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === 'PUT');
    expect(JSON.parse((put?.[1] as RequestInit).body as string)).toEqual({
      baseline: 'baseline-digest',
      patch: { 'filesystem.read_timeout': '1h30m' },
    });
  });
});

describe('ConfigPage dangerous changes', () => {
  it('asks for confirmation before saving a dangerous key and does not save when cancelled', async () => {
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/v1/config' && init?.method === 'PUT') {
        return Promise.resolve(jsonResponse({
          applied: ['cache.capacity'],
          restart_required: ['backend'],
          persisted: true,
        }));
      }
      return Promise.resolve(jsonResponse(configView()));
    });
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'backend' });
    fireEvent.change(screen.getByRole('combobox', { name: 'backend' }), { target: { value: 'fuse' } });
    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '1GiB' } });

    const preview = screen.getByTestId('config-diff-preview');
    expect(within(preview).getByText('backend')).toBeInTheDocument();
    expect(within(preview).getByText('fuse')).toBeInTheDocument();
    expect(within(preview).getByText('输出形态变化')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('危险配置项确认');
    expect(dialog).toHaveTextContent('backend');
    expect(dialog).toHaveTextContent('输出形态变化');
    expect(dialog).toHaveTextContent(/挂载点/);
    // Only the dangerous keys are listed; safe changes stay in the preview.
    expect(within(dialog).queryByText('cache.capacity')).not.toBeInTheDocument();
    expect(fetchMock.mock.calls.some(([, init]) => (init as RequestInit | undefined)?.method === 'PUT')).toBe(false);

    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(fetchMock.mock.calls.some(([, init]) => (init as RequestInit | undefined)?.method === 'PUT')).toBe(false);
    expect(screen.getByRole('combobox', { name: 'backend' })).toHaveValue('fuse');
    expect(screen.getByTestId('config-dirty-count')).toHaveTextContent('未保存 2 项');

    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    const confirm = await screen.findByRole('dialog');
    fireEvent.click(within(confirm).getByRole('button', { name: '仍然保存' }));

    await screen.findByText('保存成功');
    const putCall = fetchMock.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === 'PUT');
    expect(putCall).toBeDefined();
    expect(JSON.parse((putCall?.[1] as RequestInit).body as string)).toEqual({
      baseline: 'baseline-digest',
      patch: { backend: 'fuse', 'cache.capacity': '1GiB' },
    });
  });

  it('saves a safe change without interrupting for confirmation', async () => {
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/v1/config' && init?.method === 'PUT') {
        return Promise.resolve(jsonResponse({ applied: ['cache.capacity'], restart_required: [], persisted: true }));
      }
      return Promise.resolve(jsonResponse(configView()));
    });
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'cache.capacity' });
    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '1GiB' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await screen.findByText('保存成功');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
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

describe('ConfigPage unsaved changes', () => {
  it('warns about unsaved changes, blocks navigation and lets the user stay', async () => {
    const fetchMock = vi.fn((url: string) => {
      if (url === '/api/v1/config') {
        return Promise.resolve(jsonResponse(configView()));
      }
      if (url === '/api/v1/experiments/upload') {
        return Promise.resolve(jsonResponse(disabledExperiment()));
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    renderAppAt(fetchMock);
    await screen.findByRole('article', { name: 'cache.capacity' });

    expect(beforeUnloadEvent().defaultPrevented).toBe(false);

    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '1GiB' } });

    expect(screen.getByTestId('config-dirty-count')).toHaveTextContent('未保存 1 项');
    expect(beforeUnloadEvent().defaultPrevented).toBe(true);

    fireEvent.click(screen.getByRole('link', { name: '上传实验监控' }));
    const stayDialog = await screen.findByRole('dialog');
    expect(stayDialog).toHaveTextContent('离开配置页面');
    expect(stayDialog).toHaveTextContent('1 项未保存的改动');
    fireEvent.click(within(stayDialog).getByRole('button', { name: '留在此页' }));

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.getByRole('article', { name: 'cache.capacity' })).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'cache.capacity' })).toHaveValue('1GiB');

    fireEvent.click(screen.getByRole('link', { name: '上传实验监控' }));
    const leaveDialog = await screen.findByRole('dialog');
    fireEvent.click(within(leaveDialog).getByRole('button', { name: '放弃改动并离开' }));

    await screen.findByRole('heading', { name: '上传实验监控' });
    expect(screen.queryByRole('article', { name: 'cache.capacity' })).not.toBeInTheDocument();
  });

  it('blocks the browser back/forward buttons while dirty', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(configView())));
    const { router } = renderPage(
      fetchMock,
      configView(),
      '/config?from=2',
      ['/config', '/config?from=2'],
      1,
    );
    await screen.findByRole('article', { name: 'cache.capacity' });
    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '1GiB' } });

    // The history entry before the page is a browser-level navigation; the
    // data router blocker catches it like a link click.
    await act(async () => {
      await router.navigate(-1);
    });
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('离开配置页面');
    fireEvent.click(within(dialog).getByRole('button', { name: '留在此页' }));
    await waitFor(() => expect(router.state.location.search).toBe('?from=2'));

    await act(async () => {
      await router.navigate(-1);
    });
    const confirm = await screen.findByRole('dialog');
    fireEvent.click(within(confirm).getByRole('button', { name: '放弃改动并离开' }));
    await waitFor(() => expect(router.state.location.search).toBe(''));
  });

  it('clears the unsaved state and unload warning after a successful save', async () => {
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/v1/config' && init?.method === 'PUT') {
        return Promise.resolve(jsonResponse({ applied: ['cache.capacity'], restart_required: [], persisted: true }));
      }
      return Promise.resolve(jsonResponse(configView()));
    });
    renderPage(fetchMock);
    await screen.findByRole('article', { name: 'cache.capacity' });
    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '1GiB' } });
    expect(screen.getByTestId('config-dirty-count')).toBeInTheDocument();
    expect(beforeUnloadEvent().defaultPrevented).toBe(true);

    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    await screen.findByText('保存成功');

    await waitFor(() => expect(screen.queryByTestId('config-dirty-count')).not.toBeInTheDocument());
    expect(screen.queryByTestId('config-diff-preview')).not.toBeInTheDocument();
    expect(beforeUnloadEvent().defaultPrevented).toBe(false);
  });
});

describe('ConfigPage save result groups', () => {
  it('separates applied and restart-required keys with counts and explanations', async () => {
    const view = configView({
      keys: [
        ...configView().keys,
        configKey({
          path: 'http.max_upload_size',
          section: 'http',
          kind: 'byte_quantity',
          default: '32MiB',
          file_value: '32MiB',
          effective_value: '32MiB',
          component: 'api',
          hot_apply: true,
        }),
      ],
    });
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/v1/config' && init?.method === 'PUT') {
        return Promise.resolve(jsonResponse({
          applied: ['http.max_upload_size'],
          restart_required: ['cache.capacity'],
          persisted: true,
        }));
      }
      return Promise.resolve(jsonResponse(view));
    });
    renderPage(fetchMock, view);

    await screen.findByRole('article', { name: 'http.max_upload_size' });
    fireEvent.change(screen.getByRole('textbox', { name: 'http.max_upload_size' }), { target: { value: '64MiB' } });
    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '1GiB' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await screen.findByText('保存成功');
    expect(screen.getByTestId('config-save-summary')).toHaveTextContent('已生效 1 项，重启后生效 1 项');

    const applied = screen.getByTestId('config-applied-group');
    expect(applied).toHaveTextContent('已生效（1 项）');
    expect(applied).toHaveTextContent('已应用到运行中的进程');
    expect(within(applied).getByTestId('config-applied-keys')).toHaveTextContent('http.max_upload_size');

    const restart = screen.getByTestId('config-restart-group');
    expect(restart).toHaveTextContent('重启后生效（1 项）');
    expect(restart).toHaveTextContent('重启服务后才采用新值');
    expect(within(restart).getByTestId('config-restart-keys')).toHaveTextContent('cache.capacity');
  });
});

describe('ConfigPage external modification', () => {
  it('keeps local edits on a conflict and reloads from disk only when the user asks', async () => {
    const localView = configView();
    const remoteView = configView({
      baseline: 'remote-baseline',
      keys: localView.keys.map((key) => (key.path === 'cache.capacity'
        ? { ...key, file_value: '3GiB', effective_value: '3GiB', source: 'file' }
        : key)),
    });
    let configGets = 0;
    let conflict = true;
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/v1/config' && init?.method === 'PUT') {
        if (conflict) {
          conflict = false;
          return Promise.resolve(jsonResponse({
            error: '配置文件已被外部修改',
            code: 'conflict',
            field_errors: [],
            applied: [],
            restart_required: [],
            persisted: false,
          }, 409));
        }
        return Promise.resolve(jsonResponse({
          applied: [],
          restart_required: ['cache.capacity'],
          persisted: true,
        }));
      }
      configGets += 1;
      return Promise.resolve(jsonResponse(configGets === 1 ? localView : remoteView));
    });
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'cache.capacity' });
    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '1GiB' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await screen.findByText('保存未完成');
    expect(screen.getByText(/配置文件已被外部修改/)).toBeInTheDocument();
    expect(screen.getByText(/重新加载后才能继续保存/)).toBeInTheDocument();
    // The page must not rebase silently before the user chooses to reload.
    expect(configGets).toBe(1);
    expect(screen.getByRole('textbox', { name: 'cache.capacity' })).toHaveValue('1GiB');

    fireEvent.click(screen.getByRole('button', { name: '重新加载配置' }));

    await waitFor(() => expect(screen.getByRole('textbox', { name: 'cache.capacity' })).toHaveValue('3GiB'));
    expect(screen.queryByText('保存未完成')).not.toBeInTheDocument();
    expect(screen.queryByTestId('config-dirty-count')).not.toBeInTheDocument();
    expect(configGets).toBe(2);

    // The reloaded baseline is what the next save sends.
    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '4GiB' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    await screen.findByText('保存成功');

    const puts = fetchMock.mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method === 'PUT');
    expect(puts).toHaveLength(2);
    expect(JSON.parse((puts[1]?.[1] as RequestInit).body as string)).toEqual({
      baseline: 'remote-baseline',
      patch: { 'cache.capacity': '4GiB' },
    });
  });
});

describe('ConfigPage last save status', () => {
  it('shows the most recent successful save from the view after a refresh', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(configView({
      last_save: {
        at: '2026-11-08T12:00:00Z',
        ok: true,
        error: '',
        keys: ['cache.capacity'],
      },
    }))));
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'cache.capacity' });
    const status = screen.getByTestId('config-save-status');
    expect(status).toHaveTextContent('最近一次保存成功');
    expect(status).toHaveTextContent('cache.capacity');
    expect(within(status).getByTestId('config-save-status-time')).not.toBeEmptyDOMElement();
  });

  it('shows the most recent failed save and its reason', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(configView({
      last_save: {
        at: '2026-11-08T12:00:00Z',
        ok: false,
        error: 'config: cache.capacity: must be between 1 and the cache watermark safety limit',
        keys: [],
      },
    }))));
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'cache.capacity' });
    const status = screen.getByTestId('config-save-status');
    expect(status).toHaveTextContent('最近一次保存失败');
    expect(status).toHaveTextContent('must be between 1');
    expect(within(status).getByTestId('config-save-status-time')).not.toBeEmptyDOMElement();
  });
});

describe('ConfigPage automatic refresh', () => {
  it('polls the view while the page is visible and surfaces an automatic reload', async () => {
    let configGets = 0;
    const fetchMock = vi.fn((url: string) => {
      if (url !== '/api/v1/config') {
        throw new Error(`Unexpected request: ${url}`);
      }
      configGets += 1;
      return Promise.resolve(jsonResponse(configGets === 1
        ? configView()
        : configView({
          last_reload: {
            at: '2026-11-08T12:00:00Z',
            ok: true,
            error: '',
            keys: ['cache.capacity'],
          },
        })));
    });
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'cache.capacity' });
    expect(screen.getByTestId('config-reload-status')).toHaveTextContent('尚未检测到外部修改');

    // The page polls on the same order as the file watcher; wait for the next
    // tick rather than for a manual refresh.
    await waitFor(
      () => expect(screen.getByTestId('config-reload-status')).toHaveTextContent('最近一次自动重载成功'),
      { timeout: 4000 },
    );
    expect(configGets).toBeGreaterThanOrEqual(2);
  }, 8000);
});

describe('ConfigPage external reload status', () => {
  it('shows a successful automatic reload and marks a cold key as restart-required', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(configView({
      last_reload: {
        at: '2026-11-08T12:00:00Z',
        ok: true,
        error: '',
        keys: ['cache.capacity'],
      },
    }))));
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'cache.capacity' });
    const status = screen.getByTestId('config-reload-status');
    expect(status).toHaveTextContent('最近一次自动重载成功');
    const keys = screen.getByTestId('config-reload-keys');
    expect(keys).toHaveTextContent('cache.capacity');
    expect(keys).toHaveTextContent('重启生效');
  });

  it('shows a rejected external edit with its reason and rereads the view on demand', async () => {
    const failing = configView({
      file_error: 'config: load configuration: level "verbose" is not one of debug, info, warn, error',
      last_reload: {
        at: '2026-11-08T12:00:00Z',
        ok: false,
        error: 'config: load configuration: level "verbose" is not one of debug, info, warn, error',
        keys: [],
      },
    });
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(failing)));
    renderPage(fetchMock);

    await screen.findByTestId('config-file-error');
    expect(screen.getByTestId('config-file-error')).toHaveTextContent('继续使用最近一次成功加载的配置');
    expect(screen.getByTestId('config-file-error')).toHaveTextContent('is not one of debug, info, warn, error');
    expect(screen.getByTestId('config-reload-error')).toHaveTextContent('最近一次自动重载失败');

    // The values of the last known good configuration stay on the page: the
    // failed external edit does not remove the editing surface.
    expect(screen.getByRole('textbox', { name: 'cache.capacity' })).toHaveValue('2GiB');

    const callsBefore = fetchMock.mock.calls.length;
    fireEvent.click(screen.getByRole('button', { name: '重新读取' }));
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThan(callsBefore));
  });

  it('shows a failed reload without a file error once the file error is cleared', async () => {
    const fetchMock = vi.fn(() => Promise.resolve(jsonResponse(configView({
      last_reload: {
        at: '2026-11-08T12:00:00Z',
        ok: false,
        error: 'config: experiments.upload experiment: inject needs a rate',
        keys: [],
      },
    }))));
    renderPage(fetchMock);

    await screen.findByTestId('config-reload-status');
    expect(screen.getByTestId('config-reload-status')).toHaveTextContent('最近一次自动重载失败');
    expect(screen.getByTestId('config-reload-error')).toHaveTextContent('inject needs a rate');
    expect(screen.queryByTestId('config-file-error')).not.toBeInTheDocument();
  });
});

describe('ConfigPage reread reseeding', () => {
  function rereadViews() {
    const localView = configView();
    const remoteView = configView({
      baseline: 'remote-baseline',
      keys: localView.keys.map((key) => (key.path === 'cache.capacity'
        ? { ...key, file_value: '3GiB', effective_value: '3GiB', source: 'file' }
        : key)),
    });
    return { localView, remoteView };
  }

  it('applies the disk values from the reload status and saves against their baseline', async () => {
    const { localView, remoteView } = rereadViews();
    let configGets = 0;
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/v1/config' && init?.method === 'PUT') {
        return Promise.resolve(jsonResponse({
          applied: [],
          restart_required: ['cache.capacity'],
          persisted: true,
        }));
      }
      configGets += 1;
      return Promise.resolve(jsonResponse(configGets === 1 ? localView : remoteView));
    });
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'cache.capacity' });
    expect(screen.getByRole('textbox', { name: 'cache.capacity' })).toHaveValue('2GiB');

    // The reread lives in the reload status; it must seed the form, not just
    // refresh the view behind a form that keeps showing the old file values.
    fireEvent.click(screen.getByRole('button', { name: '重新读取' }));
    await waitFor(() => expect(screen.getByRole('textbox', { name: 'cache.capacity' })).toHaveValue('3GiB'));
    expect(screen.queryByTestId('config-dirty-count')).not.toBeInTheDocument();

    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '4GiB' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    await screen.findByText('保存成功');

    const put = fetchMock.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === 'PUT');
    expect(JSON.parse((put?.[1] as RequestInit).body as string)).toEqual({
      baseline: 'remote-baseline',
      patch: { 'cache.capacity': '4GiB' },
    });
  });

  it('asks before discarding unsaved drafts and keeps them when the reread is cancelled', async () => {
    const { localView, remoteView } = rereadViews();
    let configGets = 0;
    const fetchMock = vi.fn((url: string) => {
      if (url !== '/api/v1/config') {
        throw new Error(`Unexpected request: ${url}`);
      }
      configGets += 1;
      return Promise.resolve(jsonResponse(configGets === 1 ? localView : remoteView));
    });
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'cache.capacity' });
    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '1GiB' } });

    fireEvent.click(screen.getByRole('button', { name: '重新读取' }));
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('重新读取配置');
    expect(dialog).toHaveTextContent('1 项未保存的改动');

    fireEvent.click(within(dialog).getByRole('button', { name: '留在此页' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.getByRole('textbox', { name: 'cache.capacity' })).toHaveValue('1GiB');
    expect(configGets).toBe(1);

    fireEvent.click(screen.getByRole('button', { name: '重新读取' }));
    const confirm = await screen.findByRole('dialog');
    fireEvent.click(within(confirm).getByRole('button', { name: '放弃改动并重新读取' }));
    await waitFor(() => expect(screen.getByRole('textbox', { name: 'cache.capacity' })).toHaveValue('3GiB'));
    expect(screen.queryByTestId('config-dirty-count')).not.toBeInTheDocument();
    expect(configGets).toBe(2);
  });

  it('keeps the seeded baseline when a background refetch sees an external edit', async () => {
    const { localView, remoteView } = rereadViews();
    let configGets = 0;
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
      configGets += 1;
      return Promise.resolve(jsonResponse(configGets === 1 ? localView : remoteView));
    });
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'cache.capacity' });
    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '1GiB' } });

    // Leaving the tab and coming back refetches the view without the user
    // asking for the disk values. The draft belongs to the baseline it was
    // typed against, so a later save must conflict instead of rebasing it.
    const visibility = vi
      .spyOn(Document.prototype, 'visibilityState', 'get')
      .mockReturnValue('hidden');
    await act(async () => {
      window.dispatchEvent(new Event('visibilitychange'));
      document.dispatchEvent(new Event('visibilitychange'));
    });
    visibility.mockReturnValue('visible');
    await act(async () => {
      window.dispatchEvent(new Event('visibilitychange'));
      document.dispatchEvent(new Event('visibilitychange'));
    });

    await waitFor(() => expect(configGets).toBeGreaterThanOrEqual(2));
    expect(screen.getByRole('textbox', { name: 'cache.capacity' })).toHaveValue('1GiB');
    expect(screen.getByTestId('config-dirty-count')).toHaveTextContent('未保存 1 项');

    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    await screen.findByText('保存未完成');
    expect(screen.getByText(/配置文件已被外部修改/)).toBeInTheDocument();

    const put = fetchMock.mock.calls.find(([, init]) => (init as RequestInit | undefined)?.method === 'PUT');
    expect(JSON.parse((put?.[1] as RequestInit).body as string)).toEqual({
      baseline: 'baseline-digest',
      patch: { 'cache.capacity': '1GiB' },
    });
  });

  it('adopts the baseline of the view refreshed by a successful save', async () => {
    const localView = configView();
    const writtenView = configView({
      baseline: 'written-baseline',
      keys: localView.keys.map((key) => (key.path === 'cache.capacity'
        ? { ...key, file_value: '1GiB', source: 'file' }
        : key)),
    });
    let configGets = 0;
    const fetchMock = vi.fn((url: string, init?: RequestInit) => {
      if (url === '/api/v1/config' && init?.method === 'PUT') {
        return Promise.resolve(jsonResponse({
          applied: [],
          restart_required: ['cache.capacity'],
          persisted: true,
        }));
      }
      configGets += 1;
      return Promise.resolve(jsonResponse(configGets === 1 ? localView : writtenView));
    });
    renderPage(fetchMock);

    await screen.findByRole('article', { name: 'cache.capacity' });
    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '1GiB' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    await screen.findByText('保存成功');

    // The renderer reports the bytes it wrote; the next save must use that
    // baseline instead of conflicting with our own write.
    await waitFor(() => expect(
      within(screen.getByRole('article', { name: 'cache.capacity' })).getByTestId('config-file-value'),
    ).toHaveTextContent('1GiB'));

    fireEvent.change(screen.getByRole('textbox', { name: 'cache.capacity' }), { target: { value: '2GiB' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    await waitFor(() => expect(
      fetchMock.mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method === 'PUT'),
    ).toHaveLength(2));
    const puts = fetchMock.mock.calls.filter(([, init]) => (init as RequestInit | undefined)?.method === 'PUT');

    expect(JSON.parse((puts[1]?.[1] as RequestInit).body as string)).toEqual({
      baseline: 'written-baseline',
      patch: { 'cache.capacity': '2GiB' },
    });
  });
});
