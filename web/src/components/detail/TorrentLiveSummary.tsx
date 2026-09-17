import { Skeleton } from '@mantine/core';
import type { TorrentLiveSummary as TorrentLiveSummaryData } from '../../queries/hooks';
import { StateBadge } from '../torrents/StateBadge';
import { TorrentProgress } from '../torrents/TorrentProgress';

export function TorrentLiveSummary({ summary }: { summary?: TorrentLiveSummaryData }) {
  if (summary === undefined) {
    return (
      <section className="live-summary panel panel--padding" aria-label="Live torrent status" aria-busy="true">
        <Skeleton height={22} width="7rem" mb="md" />
        <Skeleton height={8} mb="sm" />
        <Skeleton height={16} width="12rem" />
      </section>
    );
  }

  return (
    <section className="live-summary panel panel--padding" aria-label="Live torrent status">
      <div className="live-summary__heading">
        <div>
          <p className="eyebrow">Live status</p>
          <h2>Current progress</h2>
        </div>
        <StateBadge state={summary.state} />
      </div>
      <TorrentProgress
        progress={summary.progress}
        completedBytes={summary.completedBytes}
        totalBytes={summary.totalBytes}
        state={summary.state}
      />
    </section>
  );
}
