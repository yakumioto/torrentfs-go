import { ActionIcon, Tooltip } from '@mantine/core';
import { IconTrash } from '@tabler/icons-react';
import { Link } from 'react-router-dom';
import type { Torrent } from '../../types/api';
import { formatDate } from '../../utils/format';
import { StateBadge } from './StateBadge';
import { TorrentProgress } from './TorrentProgress';
import { DeleteTorrentDialog } from '../dialogs/DeleteTorrentDialog';
import { useState } from 'react';

export function TorrentRow({ torrent }: { torrent: Torrent }) {
  const [deleteOpen, setDeleteOpen] = useState(false);
  const disabled = torrent.state === 'deleting';
  return (
    <>
      <div className="torrent-row" role="row">
        <div className="torrent-row__name" role="cell">
          <Link className="torrent-row__link" to={`/torrents/${encodeURIComponent(torrent.id)}`}>
            {torrent.name || 'Unnamed torrent'}
          </Link>
          <span className="torrent-row__hash">{torrent.info_hash || 'Info hash pending'}</span>
        </div>
        <StateBadge state={torrent.state} />
        <TorrentProgress progress={torrent.progress} completedBytes={torrent.completed_bytes} totalBytes={torrent.total_bytes} />
        <div className="torrent-row__size text-mono" title={torrent.created_at}>
          {formatDate(torrent.created_at)}
        </div>
        <div className="torrent-row__action">
          <Tooltip label={disabled ? 'Deletion in progress' : 'Delete torrent'}>
            <ActionIcon
              variant="subtle"
              color={torrent.state === 'delete_failed' ? 'coral' : 'gray'}
              aria-label={disabled ? 'Deletion in progress' : `Delete ${torrent.name}`}
              disabled={disabled}
              onClick={() => setDeleteOpen(true)}
            >
              <IconTrash size={17} />
            </ActionIcon>
          </Tooltip>
        </div>
      </div>
      <DeleteTorrentDialog torrent={torrent} opened={deleteOpen} onClose={() => setDeleteOpen(false)} />
    </>
  );
}
