import { Button, Group, Modal, Stack } from '@mantine/core';
import { useEffect, useRef, useState } from 'react';
import { IconAlertTriangle } from '@tabler/icons-react';
import type { HandoffTarget, Torrent } from '../../types/api';
import { useAuth } from '../../app/auth-context';
import { useHandoffTorrent, useOperation } from '../../queries/hooks';
import { queryKeys } from '../../queries/keys';
import { useQueryClient } from '@tanstack/react-query';
import { userFacingHandoffError } from '../../utils/user-facing-error';
import { OperationStatus } from './OperationStatus';

/**
 * Handoff moves a torrent out of TorrentFS: the target takes over, and the
 * local task is deleted once the delivery succeeds. It is not a rename of the
 * delete flow, so the copy states what happens to the local copy up front.
 */
export function HandoffTorrentDialog({ torrent, target, opened, onClose }: {
  torrent: Torrent;
  target?: HandoffTarget;
  opened: boolean;
  onClose: () => void;
}) {
  const auth = useAuth();
  const queryClient = useQueryClient();
  const handoff = useHandoffTorrent(auth.api);
  const [operationId, setOperationId] = useState('');
  const notifiedTerminal = useRef('');
  const operation = useOperation(auth.api, operationId, opened && operationId !== '');
  const state = operation.data?.state;

  useEffect(() => {
    if (state === undefined || state === 'running' || notifiedTerminal.current === state) {
      return;
    }
    notifiedTerminal.current = state;
    if (state === 'completed' || state === 'failed') {
      void queryClient.invalidateQueries({ queryKey: queryKeys.torrents });
      void queryClient.invalidateQueries({ queryKey: queryKeys.torrent(torrent.id) });
      void queryClient.invalidateQueries({ queryKey: queryKeys.torrentStatus(torrent.id) });
    }
  }, [state, queryClient, torrent.id]);

  const close = () => {
    setOperationId('');
    handoff.reset();
    notifiedTerminal.current = '';
    onClose();
  };

  const submit = () => {
    handoff.mutate({ id: torrent.id }, {
      onSuccess: (result) => setOperationId(result.operation_id),
    });
  };

  const mutationError = handoff.error;
  const busy = state === 'running' || state === 'completed';
  const targetLabel = target !== undefined && target.target !== '' ? target.target : 'Transmission';
  return (
    <Modal opened={opened} onClose={close} title="移种到 Transmission" centered closeButtonProps={{ 'aria-label': '关闭弹窗' }}>
      <Stack gap="md">
        <p className="muted" style={{ margin: 0, lineHeight: 1.55 }}>
          将 <strong>{torrent.name || '这个任务'}</strong> 移交给 <strong>{targetLabel}</strong>：TorrentFS 会暂停该任务的做种，把 .torrent 交给 Transmission，
          成功后从本机删除；移交失败会自动复原，任务保持可用。
        </p>
        {mutationError !== null && mutationError !== undefined && (
          <div className="error-callout" role="alert">
            <IconAlertTriangle size={15} aria-hidden="true" /> {userFacingHandoffError(mutationError)}
          </div>
        )}
        {operationId !== '' && <OperationStatus operation={operation.data} error={operation.error} />}
        <Group justify="flex-end">
          <Button variant="subtle" color="gray" onClick={close}>关闭</Button>
          <Button color="torrent" onClick={submit} loading={handoff.isPending} disabled={busy}>移交并删除</Button>
        </Group>
      </Stack>
    </Modal>
  );
}
