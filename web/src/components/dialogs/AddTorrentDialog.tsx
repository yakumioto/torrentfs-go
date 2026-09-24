import { Button, FileInput, Group, Modal, Stack, Tabs, Text, TextInput } from '@mantine/core';
import { useState, type DragEvent } from 'react';
import { IconMagnet, IconUpload } from '@tabler/icons-react';
import type { ApiClient } from '../../api/client';
import { ApiError } from '../../api/errors';
import { useAddTorrent, useAddTorrentFiles, type TorrentFileBatchUpdate } from '../../queries/hooks';
import { userFacingError } from '../../utils/user-facing-error';
import { mergeTorrentFiles, torrentFileKey } from './torrent-file-queue';
import styles from './AddTorrentDialog.module.css';

type FileQueueStatus = 'queued' | 'uploading' | 'succeeded' | 'failed' | 'skipped';

interface FileQueueItem {
  file: File;
  key: string;
  status: FileQueueStatus;
  torrentId?: string;
  error?: unknown;
}

interface AddTorrentDialogProps {
  opened: boolean;
  onClose: () => void;
  api: ApiClient;
  onAdded: (ids: string[]) => void;
}

const FILE_ACCEPT = '.torrent,application/x-bittorrent';

function fileErrorCopy(error: unknown): string {
  if (error instanceof ApiError) {
    switch (error.status) {
      case 400:
        return '文件不是有效的 torrent。';
      case 409:
        return '相同 info hash 的任务正在删除，请稍后重试。';
      case 413:
        return '上传的 .torrent 文件超过后台服务的大小限制。';
      case 415:
        return '后台服务仅接受 .torrent 文件上传。';
      default:
        return userFacingError(error, '后台服务未能处理这个文件。');
    }
  }
  return userFacingError(error, '后台服务未能处理这个文件。');
}

function fileStatusCopy(status: FileQueueStatus): string {
  switch (status) {
    case 'queued':
      return '等待上传';
    case 'uploading':
      return '正在上传';
    case 'succeeded':
      return '已处理，任务可用';
    case 'failed':
      return '上传失败';
    case 'skipped':
      return '未尝试';
  }
}

function fileSizeCopy(size: number): string {
  if (size < 1024) {
    return `${size} B`;
  }
  if (size < 1024 * 1024) {
    return `${(size / 1024).toFixed(1)} KiB`;
  }
  return `${(size / (1024 * 1024)).toFixed(1)} MiB`;
}

function queueItem(file: File): FileQueueItem {
  return { file, key: torrentFileKey(file), status: 'queued' };
}

export function AddTorrentDialog({ opened, onClose, api, onAdded }: AddTorrentDialogProps) {
  const [mode, setMode] = useState<'magnet' | 'file'>('magnet');
  const [magnetUri, setMagnetUri] = useState('');
  const [queue, setQueue] = useState<FileQueueItem[]>([]);
  const [invalidFiles, setInvalidFiles] = useState<File[]>([]);
  const [duplicateFiles, setDuplicateFiles] = useState<File[]>([]);
  const [dragActive, setDragActive] = useState(false);
  const [batchComplete, setBatchComplete] = useState(false);
  const [batchError, setBatchError] = useState<unknown>();
  const [batchTotal, setBatchTotal] = useState(0);
  const [batchProcessed, setBatchProcessed] = useState(0);
  const magnetMutation = useAddTorrent(api);
  const fileMutation = useAddTorrentFiles(api);
  const isBusy = magnetMutation.isPending || fileMutation.isPending;

  const resetForm = () => {
    magnetMutation.reset();
    fileMutation.reset();
    setMagnetUri('');
    setQueue([]);
    setInvalidFiles([]);
    setDuplicateFiles([]);
    setDragActive(false);
    setBatchComplete(false);
    setBatchError(undefined);
    setBatchTotal(0);
    setBatchProcessed(0);
    setMode('magnet');
  };

  const close = () => {
    if (isBusy) {
      return;
    }
    resetForm();
    onClose();
  };

  const finish = (ids: string[]) => {
    resetForm();
    onAdded([...new Set(ids)]);
  };

  const handleFileStatus = (update: TorrentFileBatchUpdate) => {
    const key = torrentFileKey(update.file);
    setQueue((current) => current.map((item) => {
      if (item.key !== key) {
        return item;
      }
      return {
        ...item,
        status: update.status,
        torrentId: update.torrent?.id ?? item.torrentId,
        error: update.status === 'failed' || update.status === 'skipped' ? update.error : undefined,
      };
    }));
    if (update.status !== 'uploading') {
      setBatchProcessed((current) => current + 1);
    }
  };

  const handleIncomingFiles = (incoming: ArrayLike<File> | Iterable<File> | null | undefined) => {
    if (isBusy) {
      return;
    }
    const result = mergeTorrentFiles(queue.map((item) => item.file), incoming);
    if (result.accepted.length > 0) {
      setQueue((current) => [...current, ...result.accepted.map(queueItem)]);
      setBatchComplete(false);
      setBatchError(undefined);
    }
    setInvalidFiles(result.invalid);
    setDuplicateFiles(result.duplicates);
  };

  const handlePickerChange = (files: File[]) => {
    if (files.length > 0) {
      handleIncomingFiles(files);
    }
  };

  const handleDragEnter = (event: DragEvent<HTMLDivElement>) => {
    event.preventDefault();
    if (!isBusy) {
      setDragActive(true);
    }
  };

  const handleDragOver = (event: DragEvent<HTMLDivElement>) => {
    event.preventDefault();
    if (!isBusy) {
      setDragActive(true);
    }
  };

  const handleDragLeave = (event: DragEvent<HTMLDivElement>) => {
    event.preventDefault();
    setDragActive(false);
  };

  const handleDrop = (event: DragEvent<HTMLDivElement>) => {
    event.preventDefault();
    setDragActive(false);
    if (!isBusy) {
      handleIncomingFiles(event.dataTransfer.files);
    }
  };

  const removeFile = (key: string) => {
    if (isBusy) {
      return;
    }
    setQueue((current) => current.filter((item) => item.key !== key));
    setBatchComplete(false);
  };

  const clearFiles = () => {
    if (isBusy) {
      return;
    }
    setQueue([]);
    setInvalidFiles([]);
    setDuplicateFiles([]);
    setBatchComplete(false);
    setBatchError(undefined);
  };

  const submitFiles = () => {
    const items = queue.filter((item) => item.status === 'queued' || item.status === 'failed' || item.status === 'skipped');
    if (items.length === 0 || isBusy) {
      return;
    }
    setBatchComplete(false);
    setBatchError(undefined);
    setBatchTotal(items.length);
    setBatchProcessed(0);
    void fileMutation.mutateAsync({ files: items.map((item) => item.file), onFileStatus: handleFileStatus })
      .then(() => setBatchComplete(true))
      .catch((error: unknown) => {
        setBatchError(error);
        setBatchComplete(true);
      });
  };

  const submit = () => {
    if (isBusy) {
      return;
    }
    if (mode === 'magnet') {
      const value = magnetUri.trim();
      if (value === '') {
        return;
      }
      magnetMutation.mutate({ magnetUri: value }, { onSuccess: (torrent) => finish([torrent.id]) });
      return;
    }
    submitFiles();
  };

  const completeBatch = () => {
    if (isBusy) {
      return;
    }
    const ids = successfulIds;
    if (ids.length === 0) {
      close();
      return;
    }
    finish(ids);
  };

  const handleModeChange = (value: string | null) => {
    if (isBusy || (value !== 'magnet' && value !== 'file')) {
      return;
    }
    setMode(value);
    magnetMutation.reset();
    setInvalidFiles([]);
    setDuplicateFiles([]);
  };

  const magnetError = magnetMutation.error;
  const magnetErrorCopy = magnetError instanceof ApiError && magnetError.status === 413
    ? '磁力链接请求超过后台服务允许的大小限制。'
    : magnetError instanceof ApiError && magnetError.status === 415
      ? '后台服务仅接受 .torrent 文件上传或磁力链接请求。'
      : userFacingError(magnetError, '后台服务拒绝了这个任务，请检查输入后重试。');
  const retryableCount = queue.filter((item) => item.status === 'failed' || item.status === 'skipped').length;
  const pendingCount = queue.filter((item) => item.status === 'queued').length;
  const successCount = queue.filter((item) => item.status === 'succeeded').length;
  const successfulIds = [...new Set(queue.flatMap((item) => item.status === 'succeeded' && item.torrentId !== undefined ? [item.torrentId] : []))];
  const uniqueSuccessCount = successfulIds.length;
  const failureCount = queue.filter((item) => item.status === 'failed' || item.status === 'skipped').length;
  const canSubmitFiles = !isBusy && retryableCount + pendingCount > 0;
  const fileSubmitLabel = retryableCount > 0 && pendingCount === 0
    ? '重试失败项'
    : retryableCount > 0
      ? '上传并重试文件'
      : '上传种子文件';

  return (
    <Modal
      opened={opened}
      onClose={close}
      title="添加任务"
      centered
      closeOnEscape={!isBusy}
      closeOnClickOutside={!isBusy}
      closeButtonProps={{ 'aria-label': '关闭弹窗', disabled: isBusy }}
    >
      <Tabs value={mode} onChange={handleModeChange}>
        <Tabs.List grow>
          <Tabs.Tab value="magnet" leftSection={<IconMagnet size={16} />} disabled={isBusy}>磁力链接</Tabs.Tab>
          <Tabs.Tab value="file" leftSection={<IconUpload size={16} />} disabled={isBusy}>种子文件</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="magnet" pt="lg">
          <Stack gap="md">
            <TextInput
              label="磁力链接"
              placeholder="magnet:?xt=urn:btih:…"
              value={magnetUri}
              onChange={(event) => setMagnetUri(event.currentTarget.value)}
              description="后台服务会在后台解析任务元数据。"
              disabled={isBusy}
            />
            <Button color="torrent" onClick={submit} loading={magnetMutation.isPending} disabled={isBusy || magnetUri.trim() === ''}>添加磁力链接</Button>
          </Stack>
        </Tabs.Panel>
        <Tabs.Panel value="file" pt="lg">
          <Stack gap="md">
            <div
              className={`${styles.dropZone} ${dragActive ? styles.dragActive : ''}`}
              data-testid="torrent-dropzone"
              onDragEnter={handleDragEnter}
              onDragOver={handleDragOver}
              onDragLeave={handleDragLeave}
              onDrop={handleDrop}
            >
              <FileInput
                label="种子文件"
                placeholder="选择一个或多个 .torrent 文件"
                accept={FILE_ACCEPT}
                multiple={true}
                value={queue.map((item) => item.file)}
                onChange={handlePickerChange}
                disabled={isBusy}
                fileInputProps={{ 'aria-label': '选择种子文件' }}
                description="可多选，也可将文件拖到此区域；浏览器会自动处理 multipart 边界。"
              />
              <Text size="sm" c="dimmed">只接受文件名以 .torrent 结尾的文件，不依赖文件 MIME 类型。</Text>
            </div>

            {invalidFiles.length > 0 && (
              <div className={styles.feedback} role="alert" aria-live="polite">
                已忽略非 .torrent 文件：{invalidFiles.map((file) => file.name).join('、')}
              </div>
            )}
            {duplicateFiles.length > 0 && (
              <div className={styles.feedback} role="alert" aria-live="polite">
                已忽略重复文件：{duplicateFiles.map((file) => file.name).join('、')}
              </div>
            )}
            {batchError !== undefined && (
              <div className={styles.feedback} role="alert">{fileErrorCopy(batchError)}</div>
            )}

            {queue.length > 0 && (
              <Stack gap="xs">
                <Group justify="space-between" align="center">
                  <Text size="sm" fw={600}>文件队列（{queue.length}）</Text>
                  <Button variant="subtle" size="xs" onClick={clearFiles} disabled={isBusy}>清空全部</Button>
                </Group>
                <ul className={styles.fileList} aria-label="已选择的种子文件">
                  {queue.map((item) => (
                    <li className={styles.fileRow} key={item.key}>
                      <div className={styles.fileDetails}>
                        <span className={styles.fileName} title={item.file.name}>{item.file.name}</span>
                        <span className={styles.fileMeta}>{fileSizeCopy(item.file.size)}</span>
                      </div>
                      <div className={styles.fileStatus} data-status={item.status} role={item.error !== undefined ? 'alert' : undefined}>
                        <span>{fileStatusCopy(item.status)}</span>
                        {item.error !== undefined && <span className={styles.fileStatusError}>{fileErrorCopy(item.error)}</span>}
                      </div>
                      <Button
                        variant="subtle"
                        color="gray"
                        size="xs"
                        onClick={() => removeFile(item.key)}
                        disabled={isBusy}
                        aria-label={`移除 ${item.file.name}`}
                      >
                        移除
                      </Button>
                    </li>
                  ))}
                </ul>
              </Stack>
            )}

            {fileMutation.isPending && (
              <div className={styles.progress} role="status" aria-live="polite">
                已处理 {batchProcessed} / {batchTotal} 个文件。
              </div>
            )}
            {batchComplete && !fileMutation.isPending && (
              <div className={styles.summary} role="status" aria-live="polite">
                已处理 {batchProcessed} / {batchTotal} 个文件。本批次结果：{successCount} 个文件成功，{uniqueSuccessCount} 个唯一任务可用，{failureCount} 个文件失败或未尝试。
                {successCount > 0 && ' 关闭不会撤销已经添加的任务。'}
              </div>
            )}

            <Group justify="flex-end">
              {batchComplete && <Button variant="default" onClick={completeBatch}>完成并关闭</Button>}
              <Button color="torrent" onClick={submit} loading={fileMutation.isPending} disabled={!canSubmitFiles}>{fileSubmitLabel}</Button>
            </Group>
          </Stack>
        </Tabs.Panel>
      </Tabs>
      {mode === 'magnet' && magnetError !== null && magnetError !== undefined && <div className="error-callout" role="alert" aria-live="polite" style={{ marginTop: '1rem' }}>{magnetErrorCopy}</div>}
    </Modal>
  );
}
