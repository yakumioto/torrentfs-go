import { Alert, Button, FileInput, Group, Modal, NativeSelect, Stack, Text } from '@mantine/core';
import { IconAlertTriangle, IconCircleCheck } from '@tabler/icons-react';
import { useEffect, useMemo, useState } from 'react';
import type { ApiClient } from '../../api/client';
import { useUploadSubtitle } from '../../queries/hooks';
import type { Subtitle, SubtitleTarget } from '../../types/api';
import { userFacingSubtitleError } from '../../utils/user-facing-error';
import { mountPathForSelection, subtitleBasenameMismatch, subtitleExtension, subtitleTargetPath } from './subtitle-name';
import styles from './UploadSubtitleDialog.module.css';

const FILE_ACCEPT = '.srt,.ass,.vtt';

export interface SubtitleUploadResult {
  replaced: boolean;
  path: string;
  mountPath: string;
}

export function UploadSubtitleDialog({
  api,
  torrentId,
  targets,
  subtitles,
  opened,
  onClose,
  onUploaded,
}: {
  api: ApiClient;
  torrentId: string;
  targets: SubtitleTarget[];
  subtitles: Subtitle[];
  opened: boolean;
  onClose: () => void;
  onUploaded?: (result: SubtitleUploadResult) => void;
}) {
  const uploadable = useMemo(() => targets.filter((target) => target.uploadable), [targets]);
  const [videoPath, setVideoPath] = useState('');
  const [file, setFile] = useState<File | null>(null);
  const [result, setResult] = useState<SubtitleUploadResult | null>(null);
  const upload = useUploadSubtitle(api);

  useEffect(() => {
    if (!opened) {
      return;
    }
    setVideoPath(uploadable.length === 1 ? uploadable[0].video_path : '');
    setFile(null);
    setResult(null);
    upload.reset();
    // upload.reset is stable for the lifetime of one mutation instance.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [opened, uploadable]);

  // With more than one candidate the user must pick explicitly: defaulting to
  // the first video would silently attach the subtitle to an arbitrary file.
  const targetOptions = uploadable.length > 1
    ? [{ value: '', label: '请选择目标视频' }, ...uploadable.map((candidate) => ({ value: candidate.video_path, label: candidate.video_path }))]
    : uploadable.map((candidate) => ({ value: candidate.video_path, label: candidate.video_path }));
  const target = uploadable.find((candidate) => candidate.video_path === videoPath);
  const nameMismatch = file !== null && target !== undefined && subtitleBasenameMismatch(target.video_path, file.name);
  const formatRejected = file !== null && subtitleExtension(file.name) === undefined;
  const relativePath = target !== undefined && file !== null ? subtitleTargetPath(target.video_path, file.name) : '';
  const replacing = relativePath !== '' && subtitles.some((subtitle) => subtitle.path === relativePath);
  const clientError = formatRejected
    ? '仅支持小写扩展名的 .srt、.ass 或 .vtt 字幕文件。'
    : nameMismatch
      ? '字幕文件名必须与所选视频的名称完全一致。'
      : '';
  const busy = upload.isPending;
  const canSubmit = !busy && target !== undefined && file !== null && clientError === '';

  const close = () => {
    if (busy) {
      return;
    }
    onClose();
  };

  const submit = () => {
    if (!canSubmit) {
      return;
    }
    upload.mutate(
      { id: torrentId, videoPath: target.video_path, file },
      {
        onSuccess: (response) => {
          const uploaded = { replaced: response.replaced, path: response.path, mountPath: response.mount_path };
          setResult(uploaded);
          onUploaded?.(uploaded);
        },
      },
    );
  };

  const failure = upload.error;
  return (
    <Modal
      opened={opened}
      onClose={close}
      title="上传字幕"
      centered
      closeOnEscape={!busy}
      closeOnClickOutside={!busy}
      closeButtonProps={{ 'aria-label': '关闭弹窗', disabled: busy }}
    >
      <Stack gap="md">
        <Text size="sm" c="dimmed">
          字幕会写入所选视频所在目录，并出现在只读挂载点中；TorrentFS 不会修改种子自带的文件。
        </Text>
        <NativeSelect
          label="目标视频"
          data={targetOptions}
          value={videoPath}
          onChange={(event) => { setVideoPath(event.currentTarget.value); setResult(null); upload.reset(); }}
          disabled={busy || uploadable.length === 0}
          description={uploadable.length === 0
            ? '这个任务没有可上传字幕的视频：没有受支持的视频文件，或视频名称在挂载点中存在冲突。'
            : '只能为列表中的视频上传与其同名的字幕。'}
        />
        <FileInput
          label="字幕文件"
          placeholder="选择 .srt、.ass 或 .vtt 文件"
          accept={FILE_ACCEPT}
          value={file}
          onChange={(value) => { setFile(value); setResult(null); upload.reset(); }}
          disabled={busy || target === undefined}
          fileInputProps={{ 'aria-label': '选择字幕文件' }}
          description="文件名（不含扩展名）必须与所选视频完全一致，且扩展名使用小写。"
        />
        {target !== undefined && (
          <div className={styles.preview}>
            <div className={styles.previewRow}>
              <span className={styles.previewLabel}>将写入</span>
              <span className="text-mono">{file === null ? subtitleTargetPath(target.video_path, `${target.expected_basename}.srt`) : relativePath}</span>
            </div>
            <div className={styles.previewRow}>
              <span className={styles.previewLabel}>挂载路径</span>
              <span className="text-mono">{file === null ? target.mount_path : mountPathForSelection(target.mount_path, file.name)}</span>
            </div>
          </div>
        )}
        {replacing && <Alert color="yellow" variant="light" title="将替换现有字幕">该路径已有由 TorrentFS 管理的字幕，提交后会原子替换为新内容。</Alert>}
        {clientError !== '' && <div className="error-callout" role="alert"><IconAlertTriangle size={15} aria-hidden="true" /> {clientError}</div>}
        {failure !== null && failure !== undefined && (
          <div className="error-callout" role="alert">{userFacingSubtitleError(failure)}</div>
        )}
        {result !== null && (
          <div className={styles.success} role="status" aria-live="polite">
            <IconCircleCheck size={18} color="var(--success)" aria-hidden="true" />
            <div>
              <div className={styles.successTitle}>{result.replaced ? '字幕已替换' : '字幕已上传'}</div>
              <div className={`${styles.successDetail} text-mono`}>{result.path}</div>
            </div>
          </div>
        )}
        <Group justify="flex-end">
          <Button variant="subtle" color="gray" onClick={close} disabled={busy}>关闭</Button>
          <Button color="torrent" onClick={submit} loading={busy} disabled={!canSubmit}>
            {replacing ? '替换字幕' : '上传字幕'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
