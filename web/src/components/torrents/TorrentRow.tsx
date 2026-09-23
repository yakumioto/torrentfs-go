import { ActionIcon, Menu, Tooltip } from '@mantine/core';
import { IconDots, IconExternalLink, IconTrash } from '@tabler/icons-react';
import { useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import type { Torrent } from '../../types/api';
import { DeleteTorrentDialog } from '../dialogs/DeleteTorrentDialog';
import styles from './TorrentRow.module.css';
import { formatBytes, formatDate } from '../../utils/format';
import { StateBadge } from './StateBadge';

export function TorrentRow({ torrent }: { torrent: Torrent }) {
  const navigate = useNavigate();
  const [deleteOpen, setDeleteOpen] = useState(false);
  const disabled = torrent.state === 'deleting';
  const name = torrent.name || '未命名任务';

  const openDelete = () => setDeleteOpen(true);

  return (
    <div className={styles.row} role="listitem">
      <Link className={styles.main} to={`/torrents/${encodeURIComponent(torrent.id)}`} aria-label={`打开任务详情：${name}`}>
        <div className={styles.name}>
          <span className={styles.title}>{name}</span>
          <span className={styles.hash}>{torrent.info_hash || '信息哈希等待生成'}</span>
        </div>
        <div className={`${styles.size} text-mono`}>
          <span className={styles.mobileLabel}>大小</span>
          <span>{formatBytes(torrent.total_bytes)}</span>
        </div>
        <div className={`${styles.download} text-mono`}>
          <span className={styles.mobileLabel}>下载量</span>
          <span>{formatBytes(torrent.downloaded_bytes)}</span>
        </div>
        <div className={`${styles.upload} text-mono`}>
          <span className={styles.mobileLabel}>上传量</span>
          <span>{formatBytes(torrent.uploaded_bytes)}</span>
        </div>
        <div className={styles.state}>
          <span className={styles.mobileLabel}>状态</span>
          <StateBadge state={torrent.state} />
        </div>
        <div className={`${styles.added} text-mono`} title={torrent.created_at}>{formatDate(torrent.created_at)}</div>
      </Link>
      <div className={styles.action}>
        <Menu shadow="md" width={220} position="bottom-end" withinPortal>
          <Menu.Target>
            <Tooltip label="更多操作">
              <ActionIcon
                variant="subtle"
                color={torrent.state === 'delete_failed' ? 'danger' : 'gray'}
                aria-label={`打开 ${name} 的更多操作`}
                disabled={disabled}
              >
                <IconDots size={18} />
              </ActionIcon>
            </Tooltip>
          </Menu.Target>
          <Menu.Dropdown>
            <Menu.Label>任务操作</Menu.Label>
            <Menu.Item leftSection={<IconExternalLink size={15} />} onClick={() => navigate(`/torrents/${encodeURIComponent(torrent.id)}`)}>
              查看详情
            </Menu.Item>
            <Menu.Divider />
            <Menu.Item color="red" leftSection={<IconTrash size={15} />} onClick={openDelete}>
              删除任务
            </Menu.Item>
          </Menu.Dropdown>
        </Menu>
      </div>
      <DeleteTorrentDialog
        torrent={torrent}
        opened={deleteOpen}
        onClose={() => setDeleteOpen(false)}
      />
    </div>
  );
}
