import { Button, FileInput, Modal, Stack, Tabs, TextInput } from '@mantine/core';
import { useState } from 'react';
import { IconMagnet, IconUpload } from '@tabler/icons-react';
import type { ApiClient } from '../../api/client';
import { ApiError, errorMessage } from '../../api/errors';
import { useAddTorrent } from '../../queries/hooks';

export function AddTorrentDialog({ opened, onClose, api, onAdded }: { opened: boolean; onClose: () => void; api: ApiClient; onAdded: (id: string) => void }) {
  const [mode, setMode] = useState<'magnet' | 'file'>('magnet');
  const [magnetUri, setMagnetUri] = useState('');
  const [file, setFile] = useState<File | null>(null);
  const mutation = useAddTorrent(api);

  const resetForm = () => {
    mutation.reset();
    setMagnetUri('');
    setFile(null);
    setMode('magnet');
  };

  const close = () => {
    if (mutation.isPending) {
      return;
    }
    resetForm();
    onClose();
  };

  const finish = (id: string) => {
    resetForm();
    onAdded(id);
  };

  const submit = () => {
    if (mode === 'magnet') {
      const value = magnetUri.trim();
      if (value === '') {
        return;
      }
      mutation.mutate({ magnetUri: value }, { onSuccess: (torrent) => finish(torrent.id) });
      return;
    }
    if (file !== null) {
      mutation.mutate({ file }, { onSuccess: (torrent) => finish(torrent.id) });
    }
  };

  const error = mutation.error;
  const errorCopy = error instanceof ApiError && error.status === 413
    ? 'That .torrent file is larger than the daemon upload limit.'
    : error instanceof ApiError && error.status === 415
      ? 'The daemon only accepts a .torrent multipart upload or a magnet JSON request.'
      : errorMessage(error, 'The daemon rejected this torrent.');

  return (
    <Modal opened={opened} onClose={close} title="Add torrent" centered>
      <Tabs value={mode} onChange={(value) => { if (value === 'magnet' || value === 'file') { setMode(value); mutation.reset(); } }}>
        <Tabs.List grow>
          <Tabs.Tab value="magnet" leftSection={<IconMagnet size={16} />}>Magnet URI</Tabs.Tab>
          <Tabs.Tab value="file" leftSection={<IconUpload size={16} />}>.torrent file</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="magnet" pt="lg">
          <Stack gap="md">
            <TextInput label="Magnet URI" placeholder="magnet:?xt=urn:btih:…" value={magnetUri} onChange={(event) => setMagnetUri(event.currentTarget.value)} description="The daemon will resolve metadata in the background." />
            <Button color="mint" onClick={submit} loading={mutation.isPending} disabled={magnetUri.trim() === ''}>Add magnet</Button>
          </Stack>
        </Tabs.Panel>
        <Tabs.Panel value="file" pt="lg">
          <Stack gap="md">
            <FileInput label="Torrent file" placeholder="Choose a .torrent file" accept=".torrent,application/x-bittorrent" value={file} onChange={setFile} clearable description="The upload uses the browser's multipart boundary." />
            <Button color="mint" onClick={submit} loading={mutation.isPending} disabled={file === null}>Upload torrent</Button>
          </Stack>
        </Tabs.Panel>
      </Tabs>
      {error !== null && error !== undefined && <div className="error-callout" role="alert" style={{ marginTop: '1rem' }}>{errorCopy}</div>}
    </Modal>
  );
}
