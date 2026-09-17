import { Button, Skeleton } from '@mantine/core';
import { IconInbox, IconRefresh, IconSearch } from '@tabler/icons-react';
import type { Torrent } from '../../types/api';
import styles from './TorrentList.module.css';
import { TorrentRow } from './TorrentRow';

export function TorrentList({ torrents, loading, error, onRetry, onAdd, totalCount = 0, hasFilters, onClearFilters }: {
  torrents?: Torrent[];
  loading: boolean;
  error: unknown;
  onRetry: () => void;
  onAdd: () => void;
  totalCount?: number;
  hasFilters?: boolean;
  onClearFilters?: () => void;
}) {
  if (loading && torrents === undefined) {
    return (
      <div className={`panel ${styles.list}`} aria-label="Loading torrents">
        {[0, 1, 2].map((item) => <Skeleton key={item} height={88} radius={0} />)}
      </div>
    );
  }

  if (error !== null && error !== undefined && torrents === undefined) {
    return (
      <div className="panel error-state" role="alert">
        <IconRefresh size={30} aria-hidden="true" />
        <h2>Task list unavailable</h2>
        <p>The daemon did not return a task list. Your existing tasks are unchanged.</p>
        <Button variant="light" color="mint" onClick={onRetry}>Try again</Button>
      </div>
    );
  }

  if (torrents !== undefined && torrents.length === 0) {
    if (hasFilters) {
      return (
        <div className="panel empty-state" role="status">
          <span className="empty-state__mark" aria-hidden="true"><IconSearch size={25} /></span>
          <h2>No torrents match</h2>
          <p>Try a different name, info hash, or status filter.</p>
          <Button variant="light" color="mint" onClick={onClearFilters}>Clear search and filters</Button>
        </div>
      );
    }
    return (
      <div className="panel empty-state">
        <span className="empty-state__mark" aria-hidden="true"><IconInbox size={25} /></span>
        <h2>No torrents on the board</h2>
        <p>Add a magnet URI or upload a .torrent file to put the first download in motion.</p>
        <Button color="mint" onClick={onAdd}>Add your first torrent</Button>
      </div>
    );
  }

  return (
    <div className={`panel ${styles.list}`} role="list" aria-label={`Torrents${totalCount > 0 ? `, ${totalCount} total` : ''}`}>
      <div className={styles.header} aria-hidden="true">
        <span>Torrent</span>
        <span>Size</span>
        <span>Progress</span>
        <span>State</span>
        <span>Added</span>
        <span>Actions</span>
      </div>
      {torrents?.map((torrent) => <TorrentRow key={torrent.id} torrent={torrent} />)}
    </div>
  );
}
