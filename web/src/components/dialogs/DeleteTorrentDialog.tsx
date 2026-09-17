import { Button, Checkbox, Group, Modal, Stack } from '@mantine/core';
import { useEffect, useRef, useState } from 'react';
import { IconAlertTriangle } from '@tabler/icons-react';
import type { Torrent } from '../../types/api';
import { ApiError, errorMessage } from '../../api/errors';
import { useAuth } from '../../app/auth-context';
import { useDeleteTorrent, useOperation } from '../../queries/hooks';
import { queryKeys } from '../../queries/keys';
import { useQueryClient } from '@tanstack/react-query';
import { OperationStatus } from './OperationStatus';

export function DeleteTorrentDialog({ torrent, opened, initialPurgeData = false, onClose }: { torrent: Torrent; opened: boolean; initialPurgeData?: boolean; onClose: () => void }) {
  const auth = useAuth();
  const queryClient = useQueryClient();
  const deletion = useDeleteTorrent(auth.api);
  const [purgeData, setPurgeData] = useState(false);
  const [confirmPurge, setConfirmPurge] = useState(false);
  const [operationId, setOperationId] = useState('');
  const notifiedTerminal = useRef('');
  const operation = useOperation(auth.api, operationId, opened && operationId !== '');

  useEffect(() => {
    if (opened) {
      setPurgeData(initialPurgeData);
      setConfirmPurge(false);
    }
  }, [initialPurgeData, opened]);

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
    setPurgeData(false);
    setConfirmPurge(false);
    setOperationId('');
    deletion.reset();
    notifiedTerminal.current = '';
    onClose();
  };

  const submit = () => {
    deletion.mutate({ id: torrent.id, purgeData }, {
      onSuccess: (result) => setOperationId(result.operation_id),
    });
  };

  const mutationError = deletion.error;
  const conflict = mutationError instanceof ApiError && mutationError.status === 409;
  return (
    <Modal opened={opened} onClose={close} title="Delete torrent" centered>
      <Stack gap="md">
        <p className="muted" style={{ margin: 0, lineHeight: 1.55 }}>Remove <strong>{torrent.name || 'this torrent'}</strong> from TorrentFS. This action cannot be undone.</p>
        <Checkbox label="Also purge managed payload data" checked={purgeData} onChange={(event) => { setPurgeData(event.currentTarget.checked); setConfirmPurge(false); }} />
        {purgeData && <Checkbox color="coral" label="I understand that only payload data managed by TorrentFS will be removed." checked={confirmPurge} onChange={(event) => setConfirmPurge(event.currentTarget.checked)} />}
        {conflict && <div className="error-callout"><IconAlertTriangle size={15} aria-hidden="true" /> A user-owned .torrent file still references this task. Remove that reference first.</div>}
        {mutationError !== null && mutationError !== undefined && !conflict && <div className="error-callout" role="alert">{errorMessage(mutationError)}</div>}
        {operationId !== '' && <OperationStatus operation={operation.data} error={operation.error} />}
        <Group justify="flex-end">
          <Button variant="subtle" color="gray" onClick={close}>Close</Button>
          <Button color="coral" onClick={submit} loading={deletion.isPending} disabled={operation.data?.state === 'deleting' || operation.data?.state === 'deleted' || (purgeData && !confirmPurge)}>{purgeData ? 'Confirm purge' : 'Delete torrent'}</Button>
        </Group>
      </Stack>
    </Modal>
  );
}
