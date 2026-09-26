import { Button, Group, Modal, NumberInput, Stack } from '@mantine/core';
import { IconAlertTriangle } from '@tabler/icons-react';
import { useState } from 'react';
import { useAuth } from '../../app/auth-context';
import { usePruneTorrents } from '../../queries/hooks';
import { userFacingError } from '../../utils/user-facing-error';

// Mirrors the server's cap: the day count is multiplied into a time.Duration,
// so a larger value would overflow rather than mean "585 years".
export const MAX_PRUNE_DAYS = 106751;

export function PruneTorrentsDialog({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const auth = useAuth();
  const prune = usePruneTorrents(auth.api);
  const [days, setDays] = useState<number | string>(30);
  const daysValue = Number(days);
  const daysValid = Number.isInteger(daysValue) && daysValue >= 1 && daysValue <= MAX_PRUNE_DAYS;

  const close = () => {
    prune.reset();
    setDays(30);
    onClose();
  };

  const submit = () => {
    if (!daysValid) {
      return;
    }
    prune.mutate({ olderThanDays: daysValue });
  };

  const result = prune.data;
  const failures = result?.failures ?? [];
  const nothingMatched = result !== undefined
    && result.operations.length === 0
    && result.excluded_favorites === 0
    && failures.length === 0;
  return (
    <Modal opened={opened} onClose={close} title="批量清理旧任务" centered closeButtonProps={{ 'aria-label': '关闭弹窗' }}>
      <Stack gap="md">
        <p className="muted" style={{ margin: 0, lineHeight: 1.55 }}>
          将删除添加时间早于 N 天的任务。<strong>已收藏的任务不会被删除</strong>，即使它们早于该时间。
        </p>
        <NumberInput
          label="天数阈值"
          aria-label="保留天数阈值"
          description="删除添加时间早于该天数的未收藏任务"
          min={1}
          max={MAX_PRUNE_DAYS}
          allowDecimal={false}
          allowNegative={false}
          value={days}
          onChange={setDays}
        />
        {prune.error !== null && prune.error !== undefined && (
          <div className="error-callout" role="alert">
            <IconAlertTriangle size={15} aria-hidden="true" />
            {userFacingError(prune.error, '批量清理请求失败，请稍后重试。')}
          </div>
        )}
        {result !== undefined && (
          <div role="status">
            {nothingMatched && <p style={{ margin: 0 }}>没有符合条件的任务。</p>}
            {result.operations.length > 0 && <p style={{ margin: 0 }}>已开始删除 {result.operations.length} 个任务。</p>}
            {result.excluded_favorites > 0 && <p style={{ margin: 0 }}>已保留 {result.excluded_favorites} 个收藏任务。</p>}
          </div>
        )}
        {failures.length > 0 && (
          <div className="error-callout" role="alert">
            <IconAlertTriangle size={15} aria-hidden="true" />
            有 {failures.length} 个符合条件的任务未能开始删除，其余任务不受影响，请稍后重试或查看服务端日志。
          </div>
        )}
        <Group justify="flex-end">
          <Button variant="subtle" color="gray" onClick={close}>关闭</Button>
          <Button color="torrent" onClick={submit} loading={prune.isPending} disabled={!daysValid}>开始清理</Button>
        </Group>
      </Stack>
    </Modal>
  );
}
