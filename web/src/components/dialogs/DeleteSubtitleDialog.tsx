import { Button, Group, Modal, Stack, Text } from '@mantine/core';
import { IconAlertTriangle } from '@tabler/icons-react';
import type { ApiClient } from '../../api/client';
import { ApiError } from '../../api/errors';
import { useDeleteSubtitle } from '../../queries/hooks';
import type { Subtitle } from '../../types/api';
import { userFacingSubtitleError } from '../../utils/user-facing-error';

/**
 * Deleting a subtitle unlinks the managed copy on the host and drops it from
 * the mount view. It never touches torrent payload, which is why the dialog
 * names the exact mount path instead of the whole torrent.
 */
export function DeleteSubtitleDialog({
  api,
  torrentId,
  subtitle,
  opened,
  onClose,
  onDeleted,
}: {
  api: ApiClient;
  torrentId: string;
  subtitle: Subtitle | null;
  opened: boolean;
  onClose: () => void;
  onDeleted?: () => void;
}) {
  const deletion = useDeleteSubtitle(api);
  const busy = deletion.isPending;

  const close = () => {
    if (busy) {
      return;
    }
    deletion.reset();
    onClose();
  };

  const submit = () => {
    if (subtitle === null || busy) {
      return;
    }
    deletion.mutate({ id: torrentId, path: subtitle.path }, {
      onSuccess: () => {
        deletion.reset();
        onDeleted?.();
      },
    });
  };

  const failure = deletion.error;
  const deleting = failure instanceof ApiError && failure.status === 409;
  return (
    <Modal
      opened={opened}
      onClose={close}
      title="删除字幕"
      centered
      closeOnEscape={!busy}
      closeOnClickOutside={!busy}
      closeButtonProps={{ 'aria-label': '关闭弹窗', disabled: busy }}
    >
      <Stack gap="md">
        <Text size="sm" c="dimmed">
          将删除 TorrentFS 管理的字幕 {subtitle !== null ? <strong>{subtitle.mount_path}</strong> : null}。种子自带的文件不受影响，删除后可以重新上传同名字幕。
        </Text>
        {deleting && <div className="error-callout" role="alert"><IconAlertTriangle size={15} aria-hidden="true" /> 任务正在删除，暂时不能修改字幕。</div>}
        {failure !== null && failure !== undefined && !deleting && (
          <div className="error-callout" role="alert">{userFacingSubtitleError(failure, '删除字幕请求失败，请稍后重试。')}</div>
        )}
        <Group justify="flex-end">
          <Button variant="subtle" color="gray" onClick={close} disabled={busy}>取消</Button>
          <Button color="danger" onClick={submit} loading={busy} disabled={subtitle === null}>删除字幕</Button>
        </Group>
      </Stack>
    </Modal>
  );
}
