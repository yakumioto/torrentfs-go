import { Button } from '@mantine/core';
import { IconActivity, IconPlus } from '@tabler/icons-react';
import { useOutletContext } from 'react-router-dom';
import { useAuth } from '../app/auth-context';
import { TorrentList } from '../components/torrents/TorrentList';
import type { AppOutletContext } from '../components/layout/AppLayout';
import { useTorrentList } from '../queries/hooks';
import { formatBytes } from '../utils/format';
import { usePageVisible } from '../utils/visibility';

export function DashboardPage() {
  const auth = useAuth();
  const visible = usePageVisible();
  const { openAddTorrent } = useOutletContext<AppOutletContext>();
  const query = useTorrentList(auth.api, auth.isReady && visible);
  const torrents = query.data;
  const active = torrents?.filter((torrent) => ['adding', 'downloading'].includes(torrent.state)).length ?? 0;
  const completed = torrents?.filter((torrent) => torrent.state === 'seeding' || torrent.progress >= 1).length ?? 0;
  const totalBytes = torrents?.reduce((total, torrent) => total + torrent.total_bytes, 0) ?? 0;

  return (
    <>
      <section className="dashboard-hero" aria-labelledby="dashboard-title">
        <div className="dashboard-hero__copy">
          <p className="eyebrow">Live control room</p>
          <h1 className="page-title" id="dashboard-title">Downloads in motion.</h1>
          <p className="page-subtitle">A clear read on every torrent, from the first magnet handshake to the final verified piece.</p>
          <div className="stats-grid" aria-label="Torrent summary">
            <div className="stat-tile"><span className="stat-tile__value">{active}</span><span className="stat-tile__label">Active now</span></div>
            <div className="stat-tile"><span className="stat-tile__value">{completed}</span><span className="stat-tile__label">Complete</span></div>
            <div className="stat-tile"><span className="stat-tile__value">{formatBytes(totalBytes)}</span><span className="stat-tile__label">On the board</span></div>
          </div>
        </div>
        <div className="dashboard-hero__radar" aria-label="Torrent activity field">
          <div className="radar-disc" aria-hidden="true">
            <span className="radar-sweep" />
            <span className="radar-blip radar-blip--one" />
            <span className="radar-blip radar-blip--two" />
            <span className="radar-blip radar-blip--three" />
            <span className="signal-label">{torrents?.length ?? 0} signals · peers unavailable</span>
          </div>
        </div>
      </section>

      <section aria-labelledby="torrent-list-title">
        <div className="section-heading">
          <div>
            <p className="eyebrow">The board</p>
            <h2 id="torrent-list-title">Torrent queue</h2>
          </div>
          <Button variant="subtle" color="mint" leftSection={<IconPlus size={16} />} onClick={openAddTorrent}>
            Add torrent
          </Button>
        </div>
        {query.isFetching && torrents !== undefined && <div className="refresh-warning" role="status"><IconActivity size={16} aria-hidden="true" /> Updating the latest snapshot…</div>}
        <TorrentList torrents={torrents} loading={query.isPending} error={query.error} onRetry={() => void query.refetch()} onAdd={openAddTorrent} />
      </section>
    </>
  );
}
