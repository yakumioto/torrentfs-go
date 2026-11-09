import { ActionIcon, Badge, Menu, Tooltip } from '@mantine/core';
import { IconDots, IconExternalLink, IconFolder, IconStar, IconStarFilled, IconTrash } from '@tabler/icons-react';
import { useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import type { ExperimentTorrentStat, Torrent } from '../../types/api';
import { useAuth } from '../../app/auth-context';
import { useSetFavorite } from '../../queries/hooks';
import { CategoryAssignmentDialog } from '../dialogs/CategoryAssignmentDialog';
import { DeleteTorrentDialog } from '../dialogs/DeleteTorrentDialog';
import styles from './TorrentRow.module.css';
import { formatBytes, formatDate } from '../../utils/format';
import { formatSignedBytes, experimentOverreportedBytes, experimentPendingRealUploadBytes } from '../../utils/experiments';
import { StateBadge } from './StateBadge';

/**
 * The row compares reported bytes with the real counter captured at the last
 * announce, not with the live counter: only the former is the value the
 * tracker actually received. The pending real upload is surfaced in the title
 * so the usually large live-vs-reported gap is explained instead of looking
 * like a reporting bug.
 */
function experimentRowTitle(experimentStat: ExperimentTorrentStat | undefined): string {
  if (experimentStat === undefined) {
    return '上传实验：这个种子还没有向 tracker 上报过上传量。';
  }
  const pending = experimentPendingRealUploadBytes(
    experimentStat.real_uploaded_bytes,
    experimentStat.real_uploaded_at_last_announce_bytes,
  );
  return `相对上次上报时真实量；待上报 ${formatBytes(pending)} 尚未计入`;
}

export function TorrentRow({ torrent, showExperiment = false, experimentStat }: {
  torrent: Torrent;
  /** When the experiment view is loaded, each row also shows reported/overreported bytes. */
  showExperiment?: boolean;
  experimentStat?: ExperimentTorrentStat;
}) {
  const auth = useAuth();
  const navigate = useNavigate();
  const favorite = useSetFavorite(auth.api);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [categoryOpen, setCategoryOpen] = useState(false);
  const disabled = torrent.state === 'deleting';
  const name = torrent.name || '未命名任务';
  const favoriteLabel = torrent.favorite ? `取消收藏 ${name}` : `收藏 ${name}`;

  const openDelete = () => setDeleteOpen(true);
  const toggleFavorite = () => favorite.mutate({ id: torrent.id, favorite: !torrent.favorite });

  return (
    <div className={styles.row} role="listitem">
      <Link className={styles.main} to={`/torrents/${encodeURIComponent(torrent.id)}`} aria-label={`打开任务详情：${name}`}>
        <div className={styles.name}>
          <span className={styles.title}>{name}</span>
          {torrent.category && <Badge size="xs" variant="light" color="torrent" leftSection={<IconFolder size={11} />}>{torrent.category}</Badge>}
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
          {showExperiment && (
            <span
              className={styles.experimentValues}
              title={experimentRowTitle(experimentStat)}
            >
              上报 {experimentStat === undefined ? '—' : formatBytes(experimentStat.reported_uploaded_bytes)}
              {' · '}
              虚报 {experimentStat === undefined ? '—' : formatSignedBytes(experimentOverreportedBytes(experimentStat.reported_uploaded_bytes, experimentStat.real_uploaded_at_last_announce_bytes))}
            </span>
          )}
        </div>
        <div className={styles.state}>
          <span className={styles.mobileLabel}>状态</span>
          <StateBadge state={torrent.state} />
        </div>
        <div className={`${styles.added} text-mono`} title={torrent.created_at}>{formatDate(torrent.created_at)}</div>
      </Link>
      <div className={styles.action}>
        <Tooltip label={favoriteLabel}>
          <ActionIcon
            variant="subtle"
            color={torrent.favorite ? 'yellow' : 'gray'}
            aria-label={favoriteLabel}
            aria-pressed={torrent.favorite}
            disabled={disabled}
            loading={favorite.isPending}
            onClick={toggleFavorite}
          >
            {torrent.favorite ? <IconStarFilled size={18} /> : <IconStar size={18} />}
          </ActionIcon>
        </Tooltip>
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
            <Menu.Item leftSection={<IconFolder size={15} />} onClick={() => setCategoryOpen(true)}>
              设置分类
            </Menu.Item>
            <Menu.Divider />
            <Menu.Item color="red" leftSection={<IconTrash size={15} />} onClick={openDelete}>
              删除任务
            </Menu.Item>
          </Menu.Dropdown>
        </Menu>
      </div>
      <CategoryAssignmentDialog
        torrent={torrent}
        opened={categoryOpen}
        onClose={() => setCategoryOpen(false)}
      />
      <DeleteTorrentDialog
        torrent={torrent}
        opened={deleteOpen}
        onClose={() => setDeleteOpen(false)}
      />
    </div>
  );
}
