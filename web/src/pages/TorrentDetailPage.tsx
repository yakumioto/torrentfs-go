import { Alert, Button, Tabs } from '@mantine/core';
import { IconAlertTriangle, IconClock, IconRefresh } from '@tabler/icons-react';
import { useCallback, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { ApiError, errorMessage } from '../api/errors';
import { useAuth } from '../app/auth-context';
import { DeleteTorrentDialog } from '../components/dialogs/DeleteTorrentDialog';
import { TorrentDetailHeader } from '../components/detail/TorrentDetailHeader';
import { TorrentFilesPanel } from '../components/detail/TorrentFilesPanel';
import { TorrentLiveSummary } from '../components/detail/TorrentLiveSummary';
import { TorrentOverview } from '../components/detail/TorrentOverview';
import { TorrentPiecesPanel } from '../components/detail/TorrentPiecesPanel';
import type { FileStatus, PieceStatus } from '../types/api';
import {
  useTorrentDetail,
  useTorrentLiveSummary,
  useTorrentStatus,
  useTorrentStatusFiles,
  useTorrentStatusMeta,
  useTorrentStatusPieces,
  useTorrentStatusTorrent,
} from '../queries/hooks';
import { usePageVisible } from '../utils/visibility';
import styles from './TorrentDetailPage.module.css';

const EMPTY_FILES: FileStatus[] = [];
const EMPTY_PIECES: PieceStatus[] = [];

export function TorrentDetailPage() {
  const { id = '' } = useParams();
  const auth = useAuth();
  const navigate = useNavigate();
  const visible = usePageVisible();
  const [deleteOpen, setDeleteOpen] = useState(false);
  const openDelete = useCallback(() => setDeleteOpen(true), []);
  const enabled = auth.isReady && visible;
  const detail = useTorrentDetail(auth.api, id, enabled);
  const status = useTorrentStatus(auth.api, id, enabled);
  const statusTorrent = useTorrentStatusTorrent(auth.api, id, enabled);
  const liveSummary = useTorrentLiveSummary(auth.api, id, enabled);
  const statusMeta = useTorrentStatusMeta(auth.api, id, enabled);
  const statusFiles = useTorrentStatusFiles(auth.api, id, enabled);
  const statusPieces = useTorrentStatusPieces(auth.api, id, enabled);

  if (detail.isPending) {
    return <div className={`panel panel--padding ${styles.loading}`} aria-label="Loading torrent detail"><div className={styles.loadingBar} /><div className={`${styles.loadingBar} ${styles.loadingBarShort}`} /><div className={styles.loadingBlock} /></div>;
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

  const detailTorrent = detail.data;
  if (detailTorrent === undefined) {
    return null;
  }
  const torrent = statusTorrent.data ?? detailTorrent;
  const files = statusFiles.data ?? EMPTY_FILES;
  const pieces = statusPieces.data ?? EMPTY_PIECES;
  const pending = torrent.state === 'adding' || statusMeta.data?.metainfoReady === false;

  return (
    <div className={styles.page}>
      <TorrentDetailHeader
        name={torrent.name}
        infoHash={torrent.info_hash}
        onDelete={openDelete}
        deleteDisabled={torrent.state === 'deleting'}
      />

      <TorrentLiveSummary summary={liveSummary.data} />

      {pending && (
        <div className={styles.pendingPanel} role="status">
          <IconClock className={styles.pendingIcon} size={22} aria-hidden="true" />
          <div><h2>Waiting for metadata</h2><p>This magnet is accepted and safe to keep open. Files and pieces will appear when the daemon resolves its metainfo.</p></div>
        </div>
      )}

      {status.error !== null && status.error !== undefined && status.data !== undefined && (
        <div className="refresh-warning" role="alert"><IconRefresh size={16} aria-hidden="true" /> The latest status snapshot failed. Showing the last valid snapshot.</div>
      )}
      {status.error !== null && status.error !== undefined && status.data === undefined && !pending && (
        <Alert color="yellow" icon={<IconAlertTriangle size={16} />} title="Status snapshot unavailable">{errorMessage(status.error)}</Alert>
      )}

      <Tabs defaultValue="overview" className={styles.tabs}>
        <Tabs.List aria-label="Torrent detail sections">
          <Tabs.Tab value="overview">Overview</Tabs.Tab>
          <Tabs.Tab value="files">Files <span className="subtle">· {files.length}</span></Tabs.Tab>
          <Tabs.Tab value="pieces">Pieces <span className="subtle">· {pieces.length}</span></Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="overview" className={styles.tabPanel}>
          <TorrentOverview torrent={torrent} summary={liveSummary.data} meta={statusMeta.data} />
        </Tabs.Panel>
        <Tabs.Panel value="files" className={styles.tabPanel}>
          <TorrentFilesPanel files={files} pieces={pieces} />
        </Tabs.Panel>
        <Tabs.Panel value="pieces" className={styles.tabPanel}>
          <TorrentPiecesPanel pieces={pieces} pieceLength={statusMeta.data?.pieceLength} />
        </Tabs.Panel>
      </Tabs>

      <DeleteTorrentDialog torrent={torrent} opened={deleteOpen} onClose={() => setDeleteOpen(false)} />
    </div>
  );
}
