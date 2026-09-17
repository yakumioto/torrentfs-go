import { Button, Skeleton } from '@mantine/core';
import { IconInbox, IconRefresh } from '@tabler/icons-react';
import type { Torrent } from '../../types/api';
import { TorrentRow } from './TorrentRow';

export function TorrentList({ torrents, loading, error, onRetry, onAdd }: {
  torrents?: Torrent[];
  loading: boolean;
  error: unknown;
  onRetry: () => void;
  onAdd: () => void;
}) {
  if (loading && torrents === undefined) {
    return (
      <div className="panel torrent-list" aria-label="Loading torrents">
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
    <div className="panel torrent-list" role="table" aria-label="Torrents">
      <div className="torrent-list__header" role="row">
        <span role="columnheader">Torrent</span>
        <span role="columnheader">State</span>
        <span role="columnheader">Progress</span>
        <span role="columnheader">Added</span>
        <span aria-hidden="true" />
      </div>
      {torrents?.map((torrent) => <TorrentRow key={torrent.id} torrent={torrent} />)}
    </div>
  );
}
