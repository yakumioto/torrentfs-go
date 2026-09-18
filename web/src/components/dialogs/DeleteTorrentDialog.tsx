import { Button, Group, Modal, Stack } from '@mantine/core';
import { useEffect, useRef, useState } from 'react';
import { IconAlertTriangle } from '@tabler/icons-react';
import type { Torrent } from '../../types/api';
import { ApiError, errorMessage } from '../../api/errors';
import { useAuth } from '../../app/auth-context';
import { useDeleteTorrent, useOperation } from '../../queries/hooks';
import { queryKeys } from '../../queries/keys';
import { useQueryClient } from '@tanstack/react-query';
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
    <Modal opened={opened} onClose={close} title="Delete torrent" centered>
      <Stack gap="md">
        <p className="muted" style={{ margin: 0, lineHeight: 1.55 }}>Remove <strong>{torrent.name || 'this torrent'}</strong> from TorrentFS. This drops the in-memory task; the cache contents are released with it. This action cannot be undone.</p>
        {conflict && <div className="error-callout"><IconAlertTriangle size={15} aria-hidden="true" /> A user-owned .torrent file still references this task. Remove that reference first.</div>}
        {mutationError !== null && mutationError !== undefined && !conflict && <div className="error-callout" role="alert">{errorMessage(mutationError)}</div>}
        {operationId !== '' && <OperationStatus operation={operation.data} error={operation.error} />}
        <Group justify="flex-end">
          <Button variant="subtle" color="gray" onClick={close}>Close</Button>
          <Button color="coral" onClick={submit} loading={deletion.isPending} disabled={operation.data?.state === 'deleting' || operation.data?.state === 'deleted'}>Delete torrent</Button>
        </Group>
      </Stack>
    </Modal>
  );
}
