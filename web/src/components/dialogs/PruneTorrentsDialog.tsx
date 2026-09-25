import { Button, Group, Modal, NumberInput, Stack } from '@mantine/core';
import { IconAlertTriangle } from '@tabler/icons-react';
import { useState } from 'react';
import { useAuth } from '../../app/auth-context';
import { usePruneTorrents } from '../../queries/hooks';
import { userFacingError } from '../../utils/user-facing-error';

export function PruneTorrentsDialog({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const auth = useAuth();
  const prune = usePruneTorrents(auth.api);
  const [days, setDays] = useState<number | string>(30);
  const daysValue = Number(days);
  const daysValid = Number.isInteger(daysValue) && daysValue >= 1;

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
            {result.operations.length === 0 && result.excluded_favorites === 0 && <p style={{ margin: 0 }}>没有符合条件的任务。</p>}
            {result.operations.length > 0 && <p style={{ margin: 0 }}>已开始删除 {result.operations.length} 个任务。</p>}
            {result.excluded_favorites > 0 && <p style={{ margin: 0 }}>已保留 {result.excluded_favorites} 个收藏任务。</p>}
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
