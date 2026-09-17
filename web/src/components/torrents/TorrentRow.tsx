import { ActionIcon, Menu, Tooltip } from '@mantine/core';
import { IconDots, IconExternalLink, IconTrash } from '@tabler/icons-react';
import { useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import type { Torrent } from '../../types/api';
import { DeleteTorrentDialog } from '../dialogs/DeleteTorrentDialog';
import styles from './TorrentRow.module.css';
import { formatBytes, formatDate } from '../../utils/format';
import { StateBadge } from './StateBadge';
import { TorrentProgress } from './TorrentProgress';

export function TorrentRow({ torrent }: { torrent: Torrent }) {
  const navigate = useNavigate();
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [deletePurgeData, setDeletePurgeData] = useState(false);
  const disabled = torrent.state === 'deleting';
  const name = torrent.name || 'Unnamed torrent';

  const openDelete = (purgeData: boolean) => {
    setDeletePurgeData(purgeData);
    setDeleteOpen(true);
  };

  return (
    <div className={styles.row} role="listitem">
      <Link className={styles.main} to={`/torrents/${encodeURIComponent(torrent.id)}`} aria-label={`Open details for ${name}`}>
        <div className={styles.name}>
          <span className={styles.title}>{name}</span>
          <span className={styles.hash}>{torrent.info_hash || 'Info hash pending'}</span>
        </div>
        <div className={`${styles.size} text-mono`}>{formatBytes(torrent.total_bytes)}</div>
        <TorrentProgress className={styles.progress} progress={torrent.progress} completedBytes={torrent.completed_bytes} totalBytes={torrent.total_bytes} state={torrent.state} />
        <StateBadge className={styles.state} state={torrent.state} />
        <div className={`${styles.added} text-mono`} title={torrent.created_at}>{formatDate(torrent.created_at)}</div>
      </Link>
      <div className={styles.action}>
        <Menu shadow="md" width={220} position="bottom-end" withinPortal>
          <Menu.Target>
            <Tooltip label="More actions">
              <ActionIcon
                variant="subtle"
                color={torrent.state === 'delete_failed' ? 'coral' : 'gray'}
                aria-label={`More actions for ${name}`}
                disabled={disabled}
              >
                <IconDots size={18} />
              </ActionIcon>
            </Tooltip>
          </Menu.Target>
          <Menu.Dropdown>
            <Menu.Label>Torrent</Menu.Label>
            <Menu.Item leftSection={<IconExternalLink size={15} />} onClick={() => navigate(`/torrents/${encodeURIComponent(torrent.id)}`)}>
              Open details
            </Menu.Item>
            <Menu.Divider />
            <Menu.Item color="red" leftSection={<IconTrash size={15} />} onClick={() => openDelete(false)}>
              Remove torrent
            </Menu.Item>
            <Menu.Item color="red" leftSection={<IconTrash size={15} />} onClick={() => openDelete(true)}>
              Remove torrent + data
            </Menu.Item>
          </Menu.Dropdown>
        </Menu>
      </div>
      <DeleteTorrentDialog
        torrent={torrent}
        opened={deleteOpen}
        initialPurgeData={deletePurgeData}
        onClose={() => setDeleteOpen(false)}
      />
    </div>
  );
}
