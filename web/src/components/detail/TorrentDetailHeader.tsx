import { Button } from '@mantine/core';
import { IconArrowLeft, IconTrash } from '@tabler/icons-react';
import { memo } from 'react';
import { Link } from 'react-router-dom';

export const TorrentDetailHeader = memo(function TorrentDetailHeader({ name, infoHash, onDelete, deleteDisabled }: {
  name: string;
  infoHash: string;
  onDelete: () => void;
  deleteDisabled: boolean;
}) {
  return (
    <div className="detail-header">
      <div className="detail-header__title">
        <Link to="/" className="back-link"><IconArrowLeft size={15} /> Torrents</Link>
        <p className="eyebrow">Torrent detail</p>
        <h1>{name || 'Unnamed torrent'}</h1>
        <span className="detail-header__hash">{infoHash || 'Info hash pending'}</span>
      </div>
      <div className="detail-header__actions">
        <Button color="coral" variant="light" leftSection={<IconTrash size={16} />} onClick={onDelete} disabled={deleteDisabled}>
          Remove
        </Button>
      </div>
    </div>
  );
});
