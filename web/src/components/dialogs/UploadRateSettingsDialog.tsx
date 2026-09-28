import { Button, Group, Loader, Modal, NumberInput, Stack, Switch, TextInput } from '@mantine/core';
import { IconAlertTriangle } from '@tabler/icons-react';
import { useEffect, useState } from 'react';
import { useAuth } from '../../app/auth-context';
import { useSetUploadRateSettings, useUploadRateSettings } from '../../queries/hooks';
import { uploadRateSettingsApplied, userFacingUploadRateError } from '../../utils/user-facing-error';

const TIME_PATTERN = /^([01]\d|2[0-3]):[0-5]\d$/;

function minutesOfDay(value: string): number | undefined {
  if (!TIME_PATTERN.test(value)) {
    return undefined;
  }
  const [hour, minute] = value.split(':');
  return Number(hour) * 60 + Number(minute);
}

export function UploadRateSettingsDialog({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const auth = useAuth();
  const query = useUploadRateSettings(auth.api, opened);
  const save = useSetUploadRateSettings(auth.api);

  const [enabled, setEnabled] = useState(false);
  const [rate, setRate] = useState<number | string>(1048576);
  const [scheduled, setScheduled] = useState(false);
  const [start, setStart] = useState('08:00');
  const [end, setEnd] = useState('22:00');
  const [loaded, setLoaded] = useState(false);

  // Seed the form from the daemon once per open so a reopened dialog never shows
  // a stale draft over the value a restart would restore. Every state write here
  // is guarded by `loaded`, so the effect cannot feed itself.
  useEffect(() => {
    if (!opened) {
      setLoaded(false);
      return;
    }
    const settings = query.data;
    if (settings === undefined || loaded) {
      return;
    }
    save.reset();
    setEnabled(settings.rate_limit_bytes_per_second > 0);
    setRate(settings.rate_limit_bytes_per_second > 0 ? settings.rate_limit_bytes_per_second : 1048576);
    setScheduled(settings.schedule !== null);
    setStart(settings.schedule?.start ?? '08:00');
    setEnd(settings.schedule?.end ?? '22:00');
    setLoaded(true);
  }, [opened, query.data, loaded, save]);

  const rateValue = Number(rate);
  const rateValid = Number.isInteger(rateValue) && rateValue > 0;
  const startMinutes = minutesOfDay(start);
  const endMinutes = minutesOfDay(end);
  const scheduleValid = startMinutes !== undefined && endMinutes !== undefined && startMinutes < endMinutes;
  const formValid = !enabled || (rateValid && (!scheduled || scheduleValid));

  const close = () => {
    save.reset();
    onClose();
  };

  const submit = () => {
    if (!formValid) {
      return;
    }
    save.mutate({
      settings: enabled
        ? {
          rate_limit_bytes_per_second: rateValue,
          schedule: scheduled ? { start, end } : null,
        }
        : { rate_limit_bytes_per_second: 0, schedule: null },
    });
  };

  const error = save.error;
  const appliedAnyway = uploadRateSettingsApplied(error);

  return (
    <Modal opened={opened} onClose={close} title="上传限速设置" centered closeButtonProps={{ 'aria-label': '关闭弹窗' }}>
      <Stack gap="md">
        <p className="muted" style={{ margin: 0, lineHeight: 1.55 }}>
          限制所有任务合计的 BitTorrent 上传速率，单位为 <strong>B/s</strong>（1 MiB/s = 1048576 B/s）。
          该上限只约束对 peer 的数据上传，不影响网页上传的 .torrent 或字幕大小。
        </p>

        {query.isLoading && <Loader size="sm" aria-label="正在读取上传限速设置" />}
        {query.isError && (
          <div className="error-callout" role="alert">
            <IconAlertTriangle size={15} aria-hidden="true" />
            无法读取当前设置，请关闭后重试。
          </div>
        )}

        <Switch
          label="启用上传限速"
          aria-label="启用上传限速"
          checked={enabled}
          disabled={query.isLoading}
          onChange={(event) => setEnabled(event.currentTarget.checked)}
        />

        {enabled && (
          <NumberInput
            label="上传上限（B/s）"
            aria-label="上传上限（B/s）"
            description="1 MiB/s = 1048576 B/s"
            min={1}
            allowDecimal={false}
            allowNegative={false}
            disabled={query.isLoading}
            value={rate}
            onChange={setRate}
            error={rateValid ? undefined : '请输入大于 0 的整数速率'}
          />
        )}

        {enabled && (
          <Switch
            label="仅在指定时段限速"
            aria-label="仅在指定时段限速"
            checked={scheduled}
            disabled={query.isLoading}
            onChange={(event) => setScheduled(event.currentTarget.checked)}
          />
        )}

        {enabled && scheduled && (
          <>
            <Group grow align="flex-start">
              <TextInput
                type="time"
                label="开始时间"
                aria-label="开始时间"
                disabled={query.isLoading}
                value={start}
                onChange={(event) => setStart(event.currentTarget.value)}
              />
              <TextInput
                type="time"
                label="结束时间"
                aria-label="结束时间"
                disabled={query.isLoading}
                value={end}
                onChange={(event) => setEnd(event.currentTarget.value)}
              />
            </Group>
            <p className="muted" style={{ margin: 0, lineHeight: 1.55 }}>
              时段按<strong>服务端本地时间</strong>解释，开始时刻包含、结束时刻不包含（{start} 生效、
              {end} 恢复不限速），且只支持同一天内的窗口。
            </p>
            {!scheduleValid && <div className="error-callout" role="alert">开始时间必须早于结束时间。</div>}
          </>
        )}

        {save.isSuccess && (
          <div role="status">已保存并立即生效。重启后会自动恢复该设置。</div>
        )}
        {error !== null && error !== undefined && (
          <div className="error-callout" role="alert">
            <IconAlertTriangle size={15} aria-hidden="true" />
            {userFacingUploadRateError(error)}
            {appliedAnyway && (
              <Button
                variant="subtle"
                size="compact-sm"
                onClick={() => void query.refetch()}
              >
                重新读取当前设置
              </Button>
            )}
          </div>
        )}

        <Group justify="flex-end">
          <Button variant="subtle" color="gray" onClick={close}>关闭</Button>
          <Button
            color="torrent"
            onClick={submit}
            loading={save.isPending}
            disabled={!formValid || query.isLoading}
          >
            保存
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
