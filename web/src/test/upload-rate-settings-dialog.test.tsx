import { MantineProvider } from '@mantine/core';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { AuthContext, type AuthContextValue } from '../app/auth-context';
import { UploadRateSettingsDialog } from '../components/dialogs/UploadRateSettingsDialog';
import { theme } from '../styles/theme';

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

const disabledSettings = { rate_limit_bytes_per_second: 0, schedule: null };
const scheduledSettings = {
  rate_limit_bytes_per_second: 1048576,
  schedule: { start: '08:00', end: '22:00' },
};

function renderDialog(fetchMock: ReturnType<typeof vi.fn>) {
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
          <UploadRateSettingsDialog opened onClose={() => undefined} />
        </AuthContext.Provider>
      </MantineProvider>
    </QueryClientProvider>,
  );
}

function settingsRequest(fetchMock: ReturnType<typeof vi.fn>, index = 0): [string, RequestInit] {
  const call = fetchMock.mock.calls[index];
  return [String(call[0]), (call[1] ?? {}) as RequestInit];
}

// The form is seeded from the response once it arrives. Saving is disabled
// while that read is in flight, so waiting for the button to enable is how a
// test knows the form shows the daemon's values rather than its defaults.
async function waitForLoadedForm() {
  await waitFor(() => expect(screen.getByRole('button', { name: '保存' })).toBeEnabled());
}

beforeEach(() => vi.unstubAllGlobals());
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('UploadRateSettingsDialog', () => {
  it('loads the current settings from the daemon', async () => {
    // The mock declares the fetch signature so mock.calls is typed as
    // [RequestInfo | URL, RequestInit?] instead of an empty tuple.
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      () => Promise.resolve(jsonResponse(scheduledSettings)),
    );
    renderDialog(fetchMock);

    await waitFor(() => expect(screen.getByLabelText('上传上限（带单位）')).toHaveValue('1MiB/s'));
    expect(screen.getByLabelText('开始时间')).toHaveValue('08:00');
    expect(screen.getByLabelText('结束时间')).toHaveValue('22:00');
    expect(screen.getByRole('switch', { name: '启用上传限速' })).toBeChecked();
    expect(screen.getByRole('switch', { name: '仅在指定时段限速' })).toBeChecked();

    const [url, init] = settingsRequest(fetchMock);
    expect(url).toBe('/api/v1/settings/upload-rate');
    expect(init.method ?? 'GET').toBe('GET');
  });

  it('parses human-readable rates and blocks missing units', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if ((init?.method ?? 'GET') === 'PUT') {
        return Promise.resolve(jsonResponse({ rate_limit_bytes_per_second: 32_000_000, schedule: null }));
      }
      return Promise.resolve(jsonResponse(disabledSettings));
    });
    renderDialog(fetchMock);

    await waitForLoadedForm();
    fireEvent.click(screen.getByRole('switch', { name: '启用上传限速' }));
    fireEvent.change(screen.getByLabelText('上传上限（带单位）'), { target: { value: '32MB' } });
    expect(screen.getByRole('button', { name: '保存' })).toBeDisabled();
    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    expect(fetchMock.mock.calls.filter(([, init]) => init?.method === 'PUT')).toHaveLength(0);

    fireEvent.change(screen.getByLabelText('上传上限（带单位）'), { target: { value: '32MB/s' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('已保存并立即生效'));

    const putCall = fetchMock.mock.calls.find(([, init]) => init?.method === 'PUT');
    expect(putCall?.[1]?.body).toBe(JSON.stringify({ rate_limit_bytes_per_second: 32_000_000, schedule: null }));
  });

  it('states the time semantics the daemon applies', async () => {
    // The mock declares the fetch signature so mock.calls is typed as
    // [RequestInfo | URL, RequestInit?] instead of an empty tuple.
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      () => Promise.resolve(jsonResponse(scheduledSettings)),
    );
    renderDialog(fetchMock);

    await waitFor(() => expect(screen.getByLabelText('开始时间')).toBeInTheDocument());
    expect(screen.getByRole('dialog')).toHaveTextContent('服务端本地时间');
    expect(screen.getByRole('dialog')).toHaveTextContent('开始时刻包含、结束时刻不包含');
  });

  it('saves an all-day limit and reports that it took effect immediately', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if ((init?.method ?? 'GET') === 'PUT') {
        return Promise.resolve(jsonResponse({ rate_limit_bytes_per_second: 2048, schedule: null }));
      }
      return Promise.resolve(jsonResponse(disabledSettings));
    });
    renderDialog(fetchMock);

    await waitForLoadedForm();
    expect(screen.getByRole('switch', { name: '启用上传限速' })).not.toBeChecked();
    fireEvent.click(screen.getByRole('switch', { name: '启用上传限速' }));
    fireEvent.change(screen.getByLabelText('上传上限（带单位）'), { target: { value: '2KiB/s' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('已保存并立即生效'));

    let putCall: [string, RequestInit] | undefined;
    for (let index = 0; index < fetchMock.mock.calls.length; index += 1) {
      const call = settingsRequest(fetchMock, index);
      if (call[1].method === 'PUT') {
        putCall = call;
        break;
      }
    }
    if (putCall === undefined) {
      throw new Error('no PUT request was made');
    }
    expect(putCall[0]).toBe('/api/v1/settings/upload-rate');
    expect(putCall[1].body).toBe(JSON.stringify({ rate_limit_bytes_per_second: 2048, schedule: null }));
  });

  it('saves a same-day window in B/s', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if ((init?.method ?? 'GET') === 'PUT') {
        return Promise.resolve(jsonResponse(scheduledSettings));
      }
      return Promise.resolve(jsonResponse(disabledSettings));
    });
    renderDialog(fetchMock);

    await waitForLoadedForm();
    expect(screen.getByRole('switch', { name: '启用上传限速' })).not.toBeChecked();
    fireEvent.click(screen.getByRole('switch', { name: '启用上传限速' }));
    fireEvent.click(screen.getByRole('switch', { name: '仅在指定时段限速' }));
    fireEvent.change(screen.getByLabelText('上传上限（带单位）'), { target: { value: '1MiB/s' } });
    fireEvent.change(screen.getByLabelText('开始时间'), { target: { value: '08:00' } });
    fireEvent.change(screen.getByLabelText('结束时间'), { target: { value: '22:00' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(screen.getByRole('status')).toHaveTextContent('已保存并立即生效'));
    let body: string | undefined;
    for (let index = 0; index < fetchMock.mock.calls.length; index += 1) {
      const call = settingsRequest(fetchMock, index);
      if (call[1].method === 'PUT') {
        body = call[1].body as string;
      }
    }
    expect(body).toBe(JSON.stringify({ rate_limit_bytes_per_second: 1048576, schedule: { start: '08:00', end: '22:00' } }));
  });

  it('disables saving when the window is reversed', async () => {
    // The mock declares the fetch signature so mock.calls is typed as
    // [RequestInfo | URL, RequestInit?] instead of an empty tuple.
    const fetchMock = vi.fn<(input: RequestInfo | URL, init?: RequestInit) => Promise<Response>>(
      () => Promise.resolve(jsonResponse(scheduledSettings)),
    );
    renderDialog(fetchMock);

    await waitFor(() => expect(screen.getByLabelText('开始时间')).toBeInTheDocument());
    fireEvent.change(screen.getByLabelText('开始时间'), { target: { value: '22:00' } });
    fireEvent.change(screen.getByLabelText('结束时间'), { target: { value: '08:00' } });

    expect(screen.getByRole('button', { name: '保存' })).toBeDisabled();
    expect(screen.getAllByRole('alert').some((node) => node.textContent?.includes('开始时间必须早于结束时间'))).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(screen.getByLabelText('开始时间')).toBeInTheDocument());
    const puts = fetchMock.mock.calls.filter(([, init]) => init?.method === 'PUT');
    expect(puts).toHaveLength(0);
  });

  it('reports a storage failure without claiming the save succeeded', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if ((init?.method ?? 'GET') === 'PUT') {
        return Promise.resolve(jsonResponse(
          { error: '上传限速设置未能写入', code: 'upload_rate_settings_storage_unavailable', applied: false },
          503,
        ));
      }
      return Promise.resolve(jsonResponse(disabledSettings));
    });
    renderDialog(fetchMock);

    await waitForLoadedForm();
    expect(screen.getByRole('switch', { name: '启用上传限速' })).not.toBeChecked();
    fireEvent.click(screen.getByRole('switch', { name: '启用上传限速' }));
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('.metadata 目录权限'));
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
    expect(screen.getByRole('switch', { name: '启用上传限速' })).toBeChecked();
  });

  it('tells the user the new limit is already in force when durability is unconfirmed', async () => {
    const reloadedSettings = { rate_limit_bytes_per_second: 2_000_000, schedule: null };
    let getCount = 0;
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if ((init?.method ?? 'GET') === 'PUT') {
        return Promise.resolve(jsonResponse(
          { error: 'durability', code: 'upload_rate_settings_durability_unconfirmed', applied: true },
          503,
        ));
      }
      const response = getCount === 0 ? disabledSettings : reloadedSettings;
      getCount += 1;
      return Promise.resolve(jsonResponse(response));
    });
    renderDialog(fetchMock);

    await waitForLoadedForm();
    expect(screen.getByRole('switch', { name: '启用上传限速' })).not.toBeChecked();
    fireEvent.click(screen.getByRole('switch', { name: '启用上传限速' }));
    fireEvent.click(screen.getByRole('button', { name: '保存' }));

    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('新规则已生效'));
    fireEvent.click(screen.getByRole('button', { name: '重新读取当前设置' }));
    await waitFor(() => expect(screen.getByLabelText('上传上限（带单位）')).toHaveValue('2MB/s'));
    expect(screen.getByRole('switch', { name: '启用上传限速' })).toBeChecked();
  });
});
