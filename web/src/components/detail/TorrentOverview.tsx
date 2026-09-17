import type { Torrent } from '../../types/api';
import type { TorrentLiveSummary as TorrentLiveSummaryData, TorrentStatusMeta } from '../../queries/hooks';
import { formatBytes, formatDate, percent } from '../../utils/format';
import { StateBadge } from '../torrents/StateBadge';

export function TorrentOverview({ torrent, summary, meta }: {
  torrent: Torrent;
  summary?: TorrentLiveSummaryData;
  meta?: TorrentStatusMeta;
}) {
  return (
    <section className="panel panel--padding detail-overview" aria-labelledby="overview-title">
      <div className="section-heading">
        <div>
          <p className="eyebrow">Snapshot</p>
          <h2 id="overview-title">Overview</h2>
        </div>
        <span className="section-heading__meta">{meta?.pieceCount.toLocaleString() ?? '—'} pieces</span>
      </div>
      <dl className="overview-grid">
        <div><dt>Info hash</dt><dd className="text-mono overview-grid__break">{torrent.info_hash || 'Pending'}</dd></div>
        <div><dt>State</dt><dd><StateBadge state={summary?.state ?? torrent.state} /></dd></div>
        <div><dt>Progress</dt><dd>{percent(summary?.progress ?? torrent.progress).toFixed(1)}%</dd></div>
        <div><dt>Total size</dt><dd>{formatBytes(summary?.totalBytes ?? torrent.total_bytes)}</dd></div>
        <div><dt>Completed</dt><dd>{formatBytes(summary?.completedBytes ?? torrent.completed_bytes)}</dd></div>
        <div><dt>Added</dt><dd>{formatDate(torrent.created_at)}</dd></div>
        <div><dt>Piece size</dt><dd>{meta === undefined ? '—' : `${formatBytes(meta.pieceLength)}`}</dd></div>
        <div><dt>Pieces</dt><dd>{meta?.pieceCount.toLocaleString() ?? '—'}</dd></div>
      </dl>
    </section>
  );
}
