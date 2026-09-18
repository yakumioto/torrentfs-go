import { Button, Group, Modal, Stack } from '@mantine/core';
import { useEffect, useRef, useState } from 'react';
import { IconAlertTriangle } from '@tabler/icons-react';
import type { Torrent } from '../../types/api';
import { ApiError } from '../../api/errors';
import { useAuth } from '../../app/auth-context';
import { useDeleteTorrent, useOperation } from '../../queries/hooks';
import { queryKeys } from '../../queries/keys';
import { useQueryClient } from '@tanstack/react-query';
import { userFacingError } from '../../utils/user-facing-error';
import { OperationStatus } from './OperationStatus';

export function DeleteTorrentDialog({ torrent, opened, onClose }: { torrent: Torrent; opened: boolean; onClose: () => void }) {
  const auth = useAuth();
  const queryClient = useQueryClient();
  const deletion = useDeleteTorrent(auth.api);
  const [operationId, setOperationId] = useState('');
  const notifiedTerminal = useRef('');
  const operation = useOperation(auth.api, operationId, opened && operationId !== '');

  useEffect(() => {
    const state = operation.data?.state;
    if (state === undefined || state === 'deleting' || notifiedTerminal.current === state) {
      return;
    }
    notifiedTerminal.current = state;
    if (state === 'deleted') {
      void queryClient.invalidateQueries({ queryKey: queryKeys.torrents });
      void queryClient.invalidateQueries({ queryKey: queryKeys.torrent(torrent.id) });
    }
  }, [operation.data?.state, queryClient, torrent.id]);

  const close = () => {
    setOperationId('');
    deletion.reset();
    notifiedTerminal.current = '';
    onClose();
  };

  const submit = () => {
    deletion.mutate({ id: torrent.id }, {
      onSuccess: (result) => setOperationId(result.operation_id),
    });
  };

  const mutationError = deletion.error;
  const conflict = mutationError instanceof ApiError && mutationError.status === 409;
  return (
    <Modal opened={opened} onClose={close} title="删除任务" centered closeButtonProps={{ 'aria-label': '关闭弹窗' }}>
      <Stack gap="md">
        <p className="muted" style={{ margin: 0, lineHeight: 1.55 }}>从 TorrentFS 中移除 <strong>{torrent.name || '这个任务'}</strong>。任务会从内存中移除，相关缓存也会随之释放；此操作无法撤销。</p>
        {conflict && <div className="error-callout" role="alert"><IconAlertTriangle size={15} aria-hidden="true" /> 当前任务仍被用户拥有的 .torrent 文件引用，请先解除该引用。</div>}
        {mutationError !== null && mutationError !== undefined && !conflict && <div className="error-callout" role="alert">{userFacingError(mutationError, '删除任务请求失败，请稍后重试。')}</div>}
        {operationId !== '' && <OperationStatus operation={operation.data} error={operation.error} />}
        <Group justify="flex-end">
          <Button variant="subtle" color="gray" onClick={close}>关闭</Button>
          <Button color="danger" onClick={submit} loading={deletion.isPending} disabled={operation.data?.state === 'deleting' || operation.data?.state === 'deleted'}>删除任务</Button>
        </Group>
      </Stack>
    </Modal>
  );
}
