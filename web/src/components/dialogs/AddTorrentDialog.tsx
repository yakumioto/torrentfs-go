import { Button, FileInput, Modal, Stack, Tabs, TextInput } from '@mantine/core';
import { useState } from 'react';
import { IconMagnet, IconUpload } from '@tabler/icons-react';
import type { ApiClient } from '../../api/client';
import { ApiError } from '../../api/errors';
import { useAddTorrent } from '../../queries/hooks';
import { userFacingError } from '../../utils/user-facing-error';

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
    ? mode === 'file'
      ? '上传的 .torrent 文件超过后台服务的大小限制。'
      : '磁力链接请求超过后台服务允许的大小限制。'
    : error instanceof ApiError && error.status === 415
      ? '后台服务仅接受 .torrent 文件上传或磁力链接请求。'
      : userFacingError(error, '后台服务拒绝了这个任务，请检查输入后重试。');

  return (
    <Modal opened={opened} onClose={close} title="添加任务" centered closeButtonProps={{ 'aria-label': '关闭弹窗' }}>
      <Tabs value={mode} onChange={(value) => { if (value === 'magnet' || value === 'file') { setMode(value); mutation.reset(); } }}>
        <Tabs.List grow>
          <Tabs.Tab value="magnet" leftSection={<IconMagnet size={16} />}>磁力链接</Tabs.Tab>
          <Tabs.Tab value="file" leftSection={<IconUpload size={16} />}>种子文件</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="magnet" pt="lg">
          <Stack gap="md">
            <TextInput label="磁力链接" placeholder="magnet:?xt=urn:btih:…" value={magnetUri} onChange={(event) => setMagnetUri(event.currentTarget.value)} description="后台服务会在后台解析任务元数据。" />
            <Button color="torrent" onClick={submit} loading={mutation.isPending} disabled={magnetUri.trim() === ''}>添加磁力链接</Button>
          </Stack>
        </Tabs.Panel>
        <Tabs.Panel value="file" pt="lg">
          <Stack gap="md">
            <FileInput label="种子文件" placeholder="选择 .torrent 文件" accept=".torrent,application/x-bittorrent" value={file} onChange={setFile} clearable clearButtonProps={{ 'aria-label': '清除已选文件' }} description="浏览器会自动处理 multipart 边界。" />
            <Button color="torrent" onClick={submit} loading={mutation.isPending} disabled={file === null}>上传种子文件</Button>
          </Stack>
        </Tabs.Panel>
      </Tabs>
      {error !== null && error !== undefined && <div className="error-callout" role="alert" aria-live="polite" style={{ marginTop: '1rem' }}>{errorCopy}</div>}
    </Modal>
  );
}
