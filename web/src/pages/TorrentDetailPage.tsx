import { Alert, Button, Skeleton, Tabs } from '@mantine/core';
import { IconAlertTriangle, IconArrowLeft, IconClock, IconRefresh } from '@tabler/icons-react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { useAuth } from '../app/auth-context';
import { DeleteTorrentDialog } from '../components/dialogs/DeleteTorrentDialog';
import { FilesTable } from '../components/detail/FilesTable';
import { PiecesMap } from '../components/detail/PiecesMap';
import { TorrentMetadata } from '../components/detail/TorrentMetadata';
import { StateBadge } from '../components/torrents/StateBadge';
import { useTorrentDetail, useTorrentStatus } from '../queries/hooks';
import { ApiError, errorMessage } from '../api/errors';
import { usePageVisible } from '../utils/visibility';
import { useState } from 'react';

export function TorrentDetailPage() {
  const { id = '' } = useParams();
  const auth = useAuth();
  const navigate = useNavigate();
  const visible = usePageVisible();
  const [deleteOpen, setDeleteOpen] = useState(false);
  const detail = useTorrentDetail(auth.api, id, auth.isReady && visible);
  const status = useTorrentStatus(auth.api, id, auth.isReady && visible);

  if (detail.isPending) {
    return <div className="panel panel--padding"><Skeleton height={52} width="60%" mb="md" /><Skeleton height={24} width="35%" mb="xl" /><Skeleton height={150} /></div>;
  }

  if (detail.error !== null && detail.error !== undefined && detail.data === undefined) {
    const missing = detail.error instanceof ApiError && detail.error.status === 404;
    return (
      <div className="panel error-state" role="alert">
        <IconAlertTriangle size={32} aria-hidden="true" />
        <h2>{missing ? 'Torrent not found' : 'Torrent detail unavailable'}</h2>
        <p>{missing ? 'It may have been deleted or the daemon restarted.' : errorMessage(detail.error)}</p>
        <Button variant="light" color="mint" onClick={() => navigate('/')}>Back to dashboard</Button>
      </div>
    );
  }

  const torrent = detail.data;
  if (torrent === undefined) {
    return null;
  }
  const pending = torrent.state === 'adding' || status.data?.metainfo_ready === false;

  return (
    <>
      <div className="detail-header">
        <div className="detail-header__title">
          <Link to="/" className="subtle" style={{ display: 'inline-flex', alignItems: 'center', gap: '0.35rem', marginBottom: '1rem', textDecoration: 'none', fontSize: '0.78rem' }}><IconArrowLeft size={15} /> Dashboard</Link>
          <p className="eyebrow">Torrent detail</p>
          <h1>{torrent.name || 'Unnamed torrent'}</h1>
          <span className="detail-header__hash">{torrent.info_hash || 'Info hash pending'}</span>
        </div>
        <div className="detail-header__actions">
          <StateBadge state={torrent.state} />
          <Button color="coral" variant="light" onClick={() => setDeleteOpen(true)} disabled={torrent.state === 'deleting'}>Delete torrent</Button>
        </div>
      </div>

      <TorrentMetadata torrent={torrent} />

      {pending && (
        <div className="pending-panel" role="status">
          <IconClock className="pending-panel__icon" size={22} aria-hidden="true" />
          <div><h2>Waiting for metadata</h2><p>This magnet is accepted and safe to keep open. Files and pieces will appear when the daemon resolves its metainfo.</p></div>
        </div>
      )}

      {status.error !== null && status.error !== undefined && status.data !== undefined && (
        <div className="refresh-warning" role="alert"><IconRefresh size={16} aria-hidden="true" /> The latest piece snapshot failed. Showing the last valid snapshot.</div>
      )}
      {status.error !== null && status.error !== undefined && status.data === undefined && !pending && (
        <Alert color="yellow" icon={<IconAlertTriangle size={16} />} title="Status snapshot unavailable">{errorMessage(status.error)}</Alert>
      )}

      {!pending && status.data !== undefined && (
        <Tabs defaultValue="files" variant="outline">
          <Tabs.List>
            <Tabs.Tab value="files">Files <span className="subtle">· {status.data.files.length}</span></Tabs.Tab>
            <Tabs.Tab value="pieces">Pieces <span className="subtle">· {status.data.pieces.length}</span></Tabs.Tab>
          </Tabs.List>
          <Tabs.Panel value="files" className="tab-panel"><div className="panel panel--padding"><div className="section-heading"><div><p className="eyebrow">Piece projection</p><h2>Files</h2></div><span className="section-heading__meta">half-open ranges</span></div><FilesTable files={status.data.files} pieces={status.data.pieces} /></div></Tabs.Panel>
          <Tabs.Panel value="pieces" className="tab-panel"><div className="panel panel--padding"><div className="section-heading"><div><p className="eyebrow">Whole torrent</p><h2>Piece map</h2></div><span className="section-heading__meta">piece length {status.data.piece_length.toLocaleString()} B</span></div><PiecesMap pieces={status.data.pieces} /></div></Tabs.Panel>
        </Tabs>
      )}

      <DeleteTorrentDialog torrent={torrent} opened={deleteOpen} onClose={() => setDeleteOpen(false)} />
    </>
  );
}
