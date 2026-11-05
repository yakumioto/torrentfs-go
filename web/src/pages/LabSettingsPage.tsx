import { Alert, Badge, Button, Group, Loader, NativeSelect, NumberInput, Stack, TextInput } from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { IconAlertTriangle, IconDeviceFloppy, IconFlask, IconInfoCircle, IconRefresh } from '@tabler/icons-react';
import type { FormEvent } from 'react';
import { useCallback, useEffect, useState } from 'react';
import { useAuth } from '../app/auth-context';
import { useLab, useSetLabSettings } from '../queries/hooks';
import type { LabMode, LabSettingsUpdate, LabView } from '../types/api';
import { formatBytes } from '../utils/format';
import {
  formatPeerSampleInterval,
  formatSignedBytes,
  labModeLabel,
  labModeOption,
  labOverreportedBytes,
  LAB_MODE_OPTIONS,
} from '../utils/lab';
import {
  formatByteQuantity,
  formatUploadRate,
  parseByteQuantity,
  parseUploadRate,
  uploadRateInputError,
} from '../utils/quantities';
import { labSettingsApplied, userFacingLabError } from '../utils/user-facing-error';
import { usePageVisible } from '../utils/visibility';
import styles from './LabSettingsPage.module.css';

const ENV_HINT = 'TORRENTFS_LAB_UPLOAD_MODE';

function capInputError(result: ReturnType<typeof parseByteQuantity>): string {
  if ('value' in result) {
    return '';
  }
  switch (result.error) {
    case 'missing_unit':
      return '上限必须包含字节单位，例如 10GiB。';
    case 'unknown_unit':
      return '仅支持 B、KB、MB、GB、TB、KiB、MiB、GiB 或 TiB。';
    case 'overflow':
      return '上限超出浏览器可精确编辑的范围。';
    case 'empty':
    case 'invalid_format':
    case 'missing_rate_suffix':
      return '请输入整数上限，例如 10GiB；不支持小数或科学计数法。';
  }
}

export function LabSettingsPage() {
  const auth = useAuth();
  const visible = usePageVisible();
  const query = useLab(auth.api, auth.isReady && visible);
  const save = useSetLabSettings(auth.api);

  const [mode, setMode] = useState<LabMode>('baseline');
  const [rate, setRate] = useState('1MiB/s');
  const [rateReadError, setRateReadError] = useState<string | undefined>();
  const [multiplier, setMultiplier] = useState<number | string>(2);
  const [jitter, setJitter] = useState<number | string>(20);
  const [cap, setCap] = useState('');
  const [capReadError, setCapReadError] = useState<string | undefined>();
  const [loaded, setLoaded] = useState(false);

  const seedForm = useCallback((view: LabView, resetMutation = true) => {
    if (resetMutation) {
      save.reset();
    }
    const settings = view.settings;
    if (settings === null) {
      return;
    }
    setMode(settings.mode);
    const formattedRate = settings.rate_bytes_per_second > 0
      ? formatUploadRate(settings.rate_bytes_per_second)
      : undefined;
    setRate(formattedRate ?? '1MiB/s');
    setRateReadError(
      settings.rate_bytes_per_second > 0 && formattedRate === undefined
        ? '当前速率超过浏览器可精确编辑的范围，无法无损编辑。'
        : undefined,
    );
    setMultiplier(settings.multiplier > 0 ? settings.multiplier : 2);
    setJitter(settings.jitter_percent);
    const formattedCap = settings.cap_bytes > 0 ? formatByteQuantity(settings.cap_bytes) : undefined;
    setCap(formattedCap ?? '');
    setCapReadError(
      settings.cap_bytes > 0 && formattedCap === undefined
        ? '当前上限超过浏览器可精确编辑的范围，无法无损编辑。'
        : undefined,
    );
    setLoaded(true);
  }, [save]);

  useEffect(() => {
    const view = query.data;
    if (view === undefined || loaded || !view.enabled) {
      return;
    }
    seedForm(view);
  }, [query.data, loaded, seedForm]);

  const enabled = query.data?.enabled === true;
  const controlsDisabled = !enabled;

  const modeOption = labModeOption(mode);
  const trimmedCap = cap.trim();
  const parsedRate = parseUploadRate(rate);
  const parsedCap = trimmedCap === '' ? undefined : parseByteQuantity(trimmedCap);
  const multiplierValue = typeof multiplier === 'number' ? multiplier : Number.NaN;
  const jitterValue = typeof jitter === 'number' ? jitter : Number.NaN;

  const rateValid = mode !== 'inject'
    || (rateReadError === undefined && 'value' in parsedRate && parsedRate.value > 0);
  const rateError = mode === 'inject' && !rateValid
    ? rateReadError ?? uploadRateInputError(parsedRate)
    : undefined;
  const multiplierValid = mode !== 'amplify'
    || (Number.isFinite(multiplierValue) && multiplierValue > 1);
  const jitterValid = Number.isFinite(jitterValue)
    && Number.isInteger(jitterValue)
    && jitterValue >= 0
    && jitterValue <= 100;
  const capError = capReadError ?? (parsedCap !== undefined && !('value' in parsedCap)
    ? capInputError(parsedCap)
    : undefined);
  const capValid = capError === undefined;
  const formValid = enabled && rateValid && multiplierValid && jitterValid && capValid;

  // A failed refetch still resolves with cached data, so only a successful
  // read may reseed the form; otherwise a transient failure would replace the
  // user's draft with stale values.
  const reload = async () => {
    const result = await query.refetch();
    if (result.isSuccess && result.data !== undefined) {
      seedForm(result.data);
    }
  };

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!formValid) {
      return;
    }
    const update: LabSettingsUpdate = {
      mode,
      rate_bytes_per_second: mode === 'inject' && 'value' in parsedRate ? parsedRate.value : 0,
      multiplier: mode === 'amplify' && Number.isFinite(multiplierValue) ? multiplierValue : 0,
      jitter_percent: jitterValue,
      cap_bytes: parsedCap !== undefined && 'value' in parsedCap ? parsedCap.value : 0,
    };
    save.mutate({ settings: update }, {
      onSuccess: (saved) => {
        seedForm(saved, false);
        notifications.show({
          color: 'teal',
          title: '上传实验设置已保存',
          message: `${labModeLabel(saved.settings?.mode)}已立即生效。`,
        });
      },
    });
  };

  const error = save.error;
  const appliedAnyway = labSettingsApplied(error);
  const totals = query.data?.totals;

  return (
    <div className={styles.page}>
      <section className={styles.heading} aria-labelledby="lab-title">
        <div>
          <p className="eyebrow">反作弊测试</p>
          <h1 className={styles.title} id="lab-title">
            <IconFlask size={24} aria-hidden="true" /> 上传实验设置
          </h1>
          <p className={styles.subtitle}>
            用于自建 tracker 反作弊测试的实验功能：按实验模式改写向 tracker 上报的上传量，
            并记录真实传输与上报值之间的差异。请只在受控环境中使用。
          </p>
        </div>
        {enabled && (
          <Badge size="lg" variant="light" color="torrent">{labModeLabel(query.data?.settings?.mode)}</Badge>
        )}
      </section>

      {query.isPending && (
        <div className={`panel panel--padding ${styles.loading}`} role="status" aria-busy="true" aria-label="正在读取上传实验状态">
          <Loader size="sm" />
          <span>正在读取上传实验状态…</span>
        </div>
      )}

      {query.isError && query.data === undefined && !query.isPending && (
        <Alert color="yellow" icon={<IconAlertTriangle size={16} />} title="无法读取上传实验状态" className={styles.alert}>
          <Group justify="space-between" gap="sm">
            <span>后台服务暂时没有返回实验状态，请稍后重试。</span>
            <Button variant="light" color="torrent" size="compact-sm" onClick={() => void reload()}>重新读取</Button>
          </Group>
        </Alert>
      )}

      {query.data !== undefined && !query.data.enabled && (
        <Alert color="yellow" icon={<IconInfoCircle size={16} />} title="上传实验未启用" className={styles.alert}>
          <p className={styles.alertText}>
            上传实验需要在后台服务启动时通过环境变量开启。请设置
            <code className={styles.code}>{ENV_HINT}</code>
            为 baseline、inject 或 amplify，并按需设置
            <code className={styles.code}>TORRENTFS_LAB_UPLOAD_RATE</code>、
            <code className={styles.code}>TORRENTFS_LAB_UPLOAD_MULTIPLIER</code> 等变量，然后重启服务。
            启用前表单不可编辑。
          </p>
        </Alert>
      )}

      <div className={styles.layout}>
        <form className={`panel panel--padding ${styles.form}`} aria-label="上传实验设置表单" onSubmit={submit}>
          <Stack gap="md">
            <div className="section-heading">
              <div>
                <p className="eyebrow">实验配置</p>
                <h2>上报模型</h2>
              </div>
            </div>

            <NativeSelect
              label="实验模式"
              aria-label="实验模式"
              data={LAB_MODE_OPTIONS.map(({ value, label }) => ({
                value,
                label: `${value} — ${label}`,
              }))}
              value={mode}
              onChange={(event) => setMode(event.currentTarget.value as LabMode)}
              disabled={controlsDisabled}
            />
            <p className={styles.modeHint}>{modeOption?.description}</p>

            {mode === 'inject' && (
              <TextInput
                label="虚报速率"
                aria-label="虚报速率"
                description="带字节单位，例如 2MiB/s；即平均每秒虚增的上报字节数。"
                disabled={controlsDisabled}
                value={rate}
                error={rateError}
                onChange={(event) => {
                  setRateReadError(undefined);
                  setRate(event.currentTarget.value);
                }}
              />
            )}

            {mode === 'amplify' && (
              <NumberInput
                label="放大倍率"
                aria-label="放大倍率"
                description="上报增量 = 真实上传增量 × 倍率，必须大于 1。"
                disabled={controlsDisabled}
                min={1}
                step={0.5}
                decimalScale={3}
                value={multiplier}
                error={mode === 'amplify' && !multiplierValid ? '倍率必须是大于 1 的数值。' : undefined}
                onChange={setMultiplier}
              />
            )}

            <NumberInput
              label="抖动百分比"
              aria-label="抖动百分比"
              description="每次 announce 相对标称增量的随机偏差，0 表示完全确定。"
              disabled={controlsDisabled}
              min={0}
              max={100}
              step={1}
              clampBehavior="strict"
              value={jitter}
              error={jitterValid ? undefined : '抖动百分比必须是 0 到 100 之间的整数。'}
              onChange={setJitter}
            />

            <TextInput
              label="上报上限（可选）"
              aria-label="上报上限（可选）"
              description="累计上报的封顶值，留空或 0 表示不封顶。"
              placeholder="例如 10GiB，留空则不封顶"
              disabled={controlsDisabled}
              value={cap}
              error={capError}
              onChange={(event) => {
                setCapReadError(undefined);
                setCap(event.currentTarget.value);
              }}
            />

            {error !== null && error !== undefined && (
              <div className="error-callout" role="alert">
                <IconAlertTriangle size={15} aria-hidden="true" />
                {userFacingLabError(error)}
                {appliedAnyway && (
                  <Button variant="subtle" size="compact-sm" onClick={() => void reload()}>
                    重新读取当前设置
                  </Button>
                )}
              </div>
            )}

            <Group justify="flex-end">
              <Button
                type="submit"
                color="torrent"
                leftSection={<IconDeviceFloppy size={17} />}
                loading={save.isPending}
                disabled={!formValid}
              >
                保存设置
              </Button>
            </Group>
          </Stack>
        </form>

        <aside className={`panel panel--padding ${styles.meta}`} aria-label="实验运行信息">
          <div className="section-heading">
            <div>
              <p className="eyebrow">运行信息</p>
              <h2>当前实验</h2>
            </div>
          </div>
          <dl className={styles.metaList}>
            <div><dt>实验模式</dt><dd>{labModeLabel(query.data?.settings?.mode)}</dd></div>
            <div><dt>随机种子</dt><dd className="text-mono">{enabled ? query.data?.seed : '—'}</dd></div>
            <div><dt>Announce 日志</dt><dd className="text-mono">{enabled && query.data?.announce_log ? query.data.announce_log : '—'}</dd></div>
            <div><dt>Peer 采样间隔</dt><dd>{enabled ? formatPeerSampleInterval(query.data?.peer_sample_interval_seconds ?? 0) : '—'}</dd></div>
            <div><dt>本次启动真实上传</dt><dd className="text-mono">{formatBytes(totals?.real_uploaded_bytes ?? 0)}</dd></div>
            <div><dt>Tracker 上报</dt><dd className="text-mono">{formatBytes(totals?.reported_uploaded_bytes ?? 0)}</dd></div>
            <div><dt>虚报量</dt><dd className="text-mono">{formatSignedBytes(labOverreportedBytes(totals?.reported_uploaded_bytes ?? 0, totals?.real_uploaded_bytes ?? 0))}</dd></div>
          </dl>
          {!enabled && (
            <p className={styles.metaEmpty}>启用实验后，这里会显示种子、日志路径与实时统计。</p>
          )}
          <Group gap="xs" className={styles.refreshRow}>
            <Button variant="subtle" size="compact-sm" leftSection={<IconRefresh size={15} />} onClick={() => void reload()} loading={query.isFetching && !query.isPending}>
              刷新状态
            </Button>
          </Group>
        </aside>
      </div>
    </div>
  );
}
