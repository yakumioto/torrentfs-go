import { Button, Group, Modal, NativeSelect, Stack } from '@mantine/core';
import { IconAlertTriangle } from '@tabler/icons-react';
import { useEffect, useState } from 'react';
import type { Torrent } from '../../types/api';
import { useAuth } from '../../app/auth-context';
import { useCategories, useSetTorrentCategory } from '../../queries/hooks';
import { userFacingError } from '../../utils/user-facing-error';

const UNCLASSIFIED = '__unclassified__';

export function CategoryAssignmentDialog({ torrent, opened, onClose }: { torrent: Torrent; opened: boolean; onClose: () => void }) {
  const auth = useAuth();
  const categories = useCategories(auth.api, opened && auth.isReady);
  const update = useSetTorrentCategory(auth.api);
  const [selected, setSelected] = useState(torrent.category);

  useEffect(() => {
    if (opened) {
      setSelected(torrent.category);
    }
  }, [opened, torrent.category]);

  const close = () => {
    update.reset();
    onClose();
  };

  const submit = () => {
    update.mutate({ id: torrent.id, category: selected }, { onSuccess: close });
  };

  const options = [
    { value: UNCLASSIFIED, label: '未分类' },
    ...(categories.data ?? []).map((category) => ({ value: category.name, label: category.name })),
  ];

  return (
    <Modal opened={opened} onClose={close} title={`设置分类：${torrent.name || '未命名任务'}`} centered closeButtonProps={{ 'aria-label': '关闭弹窗' }}>
      <Stack gap="md">
        <NativeSelect
          label="所属分类"
          data={options}
          value={selected || UNCLASSIFIED}
          onChange={(event) => {
            const value = event.currentTarget.value;
            setSelected(value === UNCLASSIFIED ? '' : value);
          }}
          disabled={categories.isPending || update.isPending}
        />
        {categories.error !== null && categories.error !== undefined && (
          <div className="error-callout" role="alert">
            <IconAlertTriangle size={15} aria-hidden="true" />
            {userFacingError(categories.error, '分类列表暂不可用。')}
          </div>
        )}
        {update.error !== null && update.error !== undefined && (
          <div className="error-callout" role="alert">
            <IconAlertTriangle size={15} aria-hidden="true" />
            {userFacingError(update.error, '更新分类失败，请稍后重试。')}
          </div>
        )}
        <Group justify="flex-end">
          <Button variant="subtle" color="gray" onClick={close}>取消</Button>
          <Button color="torrent" onClick={submit} loading={update.isPending} disabled={categories.isPending}>保存分类</Button>
        </Group>
      </Stack>
    </Modal>
  );
}
