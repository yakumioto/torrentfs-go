import { Button } from '@mantine/core';
import { IconArrowLeft, IconTrash } from '@tabler/icons-react';
import { memo } from 'react';
import { Link } from 'react-router-dom';
import styles from './TorrentDetailHeader.module.css';

export const TorrentDetailHeader = memo(function TorrentDetailHeader({ name, infoHash, onDelete, deleteDisabled }: {
  name: string;
  infoHash: string;
  onDelete: () => void;
  deleteDisabled: boolean;
}) {
  return (
    <div className={styles.header}>
      <div className={styles.title}>
        <Link to="/" className={styles.backLink}><IconArrowLeft size={15} /> Torrents</Link>
        <p className="eyebrow">Torrent detail</p>
        <h1>{name || 'Unnamed torrent'}</h1>
        <span className={styles.hash}>{infoHash || 'Info hash pending'}</span>
      </div>
      <div className={styles.actions}>
        <Button color="coral" variant="light" leftSection={<IconTrash size={16} />} onClick={onDelete} disabled={deleteDisabled}>
          Remove
        </Button>
      </div>
    </div>
  );
});
