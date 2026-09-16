import type { Torrent } from '../../types/api';
import { formatBytes, formatDate, percent } from '../../utils/format';
import { StateBadge } from '../torrents/StateBadge';
import { TorrentProgress } from '../torrents/TorrentProgress';

export function TorrentMetadata({ torrent }: { torrent: Torrent }) {
  return (
    <>
      <div className="metadata-grid" aria-label="Torrent metadata">
        <div className="metadata-cell"><span className="metadata-cell__label">State</span><StateBadge state={torrent.state} /></div>
        <div className="metadata-cell"><span className="metadata-cell__label">Size</span><span className="metadata-cell__value">{formatBytes(torrent.total_bytes)}</span></div>
        <div className="metadata-cell"><span className="metadata-cell__label">Completed</span><span className="metadata-cell__value">{formatBytes(torrent.completed_bytes)}</span></div>
        <div className="metadata-cell"><span className="metadata-cell__label">Added</span><span className="metadata-cell__value">{formatDate(torrent.created_at)}</span></div>
      </div>
      <div className="panel panel--padding" style={{ marginBottom: '1.8rem' }}>
        <div className="section-heading"><h2>Aggregate progress</h2><span className="section-heading__meta">{percent(torrent.progress).toFixed(1)}%</span></div>
        <TorrentProgress progress={torrent.progress} completedBytes={torrent.completed_bytes} totalBytes={torrent.total_bytes} />
      </div>
    </>
  );
}
