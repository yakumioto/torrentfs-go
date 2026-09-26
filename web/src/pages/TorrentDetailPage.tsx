import { Alert, Button, Tabs } from '@mantine/core';
import { IconAlertTriangle, IconClock, IconRefresh } from '@tabler/icons-react';
import { useCallback, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { ApiError } from '../api/errors';
import { useAuth } from '../app/auth-context';
import { DeleteTorrentDialog } from '../components/dialogs/DeleteTorrentDialog';
import { UploadSubtitleDialog } from '../components/dialogs/UploadSubtitleDialog';
import { TorrentDetailHeader } from '../components/detail/TorrentDetailHeader';
import { TorrentFilesPanel } from '../components/detail/TorrentFilesPanel';
import { TorrentLiveSummary } from '../components/detail/TorrentLiveSummary';
import { TorrentOverview } from '../components/detail/TorrentOverview';
import { TorrentPiecesPanel } from '../components/detail/TorrentPiecesPanel';
import type { FileStatus, PieceStatus, Subtitle } from '../types/api';
import {
  useTorrentDetail,
  useTorrentLiveSummary,
  useTorrentStatus,
  useTorrentStatusFiles,
  useTorrentStatusMeta,
  useTorrentStatusPieces,
  useTorrentStatusSubtitles,
  useTorrentStatusTorrent,
  useTorrentSubtitleTargets,
} from '../queries/hooks';
import { userFacingError } from '../utils/user-facing-error';
import { usePageVisible } from '../utils/visibility';
import styles from './TorrentDetailPage.module.css';

const EMPTY_FILES: FileStatus[] = [];
const EMPTY_PIECES: PieceStatus[] = [];
const EMPTY_SUBTITLES: Subtitle[] = [];

/**
 * A disabled upload button always carries a reason, so the control never looks
 * broken without explaining what would make it work.
 */
function subtitleUploadDisabledReason(input: { statusLoaded: boolean; deleting: boolean; pending: boolean; uploadableCount: number }): string {
  if (!input.statusLoaded) {
    return '正在读取任务状态，稍后即可上传字幕。';
  }
  if (input.deleting) {
    return '任务正在删除，无法再上传字幕。';
  }
  if (input.pending) {
    return '正在等待元数据，视频列表就绪后才能上传字幕。';
  }
  if (input.uploadableCount === 0) {
    return '这个任务没有可上传字幕的视频：要么没有受支持的视频文件，要么视频名称在挂载点中存在冲突。';
  }
  return '';
}

export function TorrentDetailPage() {
  const { id = '' } = useParams();
  const auth = useAuth();
  const navigate = useNavigate();
  const visible = usePageVisible();
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [subtitleOpen, setSubtitleOpen] = useState(false);
  const openDelete = useCallback(() => setDeleteOpen(true), []);
  const openSubtitle = useCallback(() => setSubtitleOpen(true), []);
  const enabled = auth.isReady && visible;
  const detail = useTorrentDetail(auth.api, id, enabled);
  const status = useTorrentStatus(auth.api, id, enabled);
  const statusTorrent = useTorrentStatusTorrent(auth.api, id, enabled);
  const liveSummary = useTorrentLiveSummary(auth.api, id, enabled);
  const statusMeta = useTorrentStatusMeta(auth.api, id, enabled);
  const statusFiles = useTorrentStatusFiles(auth.api, id, enabled);
  const statusPieces = useTorrentStatusPieces(auth.api, id, enabled);
  const statusSubtitles = useTorrentStatusSubtitles(auth.api, id, enabled);
  const subtitleTargets = useTorrentSubtitleTargets(auth.api, id, enabled);

  if (detail.isPending) {
    return <div className={`panel panel--padding ${styles.loading}`} role="status" aria-busy="true" aria-label="正在加载任务详情"><div className={styles.loadingBar} /><div className={`${styles.loadingBar} ${styles.loadingBarShort}`} /><div className={styles.loadingBlock} /></div>;
  }

  if (detail.error !== null && detail.error !== undefined && detail.data === undefined) {
    const missing = detail.error instanceof ApiError && detail.error.status === 404;
    return (
      <div className="panel error-state" role="alert">
        <IconAlertTriangle size={32} aria-hidden="true" />
        <h2>{missing ? '任务不存在' : '任务详情暂不可用'}</h2>
        <p>{missing ? '任务可能已经被删除，或后台服务已经重启。' : userFacingError(detail.error, '暂时无法读取任务详情，请稍后重试。')}</p>
        <Button variant="light" color="torrent" onClick={() => navigate('/')}>返回任务列表</Button>
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
  const subtitles = statusSubtitles.data ?? EMPTY_SUBTITLES;
  const targets = subtitleTargets.data ?? [];
  const pending = torrent.state === 'adding' || statusMeta.data?.metainfoReady === false;
  const uploadableTargets = targets.filter((target) => target.uploadable);
  const uploadDisabledReason = subtitleUploadDisabledReason({
    statusLoaded: subtitleTargets.data !== undefined,
    deleting: torrent.state === 'deleting' || torrent.state === 'delete_failed',
    pending,
    uploadableCount: uploadableTargets.length,
  });
  const uploadDisabled = uploadDisabledReason !== '';

  return (
    <div className={styles.page}>
      <TorrentDetailHeader
        name={torrent.name}
        infoHash={torrent.info_hash}
        onDelete={openDelete}
        deleteDisabled={torrent.state === 'deleting'}
        onUploadSubtitle={openSubtitle}
        uploadDisabled={uploadDisabled}
        uploadDisabledReason={uploadDisabledReason}
      />

      <TorrentLiveSummary summary={liveSummary.data} />

      {pending && (
        <div className={styles.pendingPanel} role="status">
          <IconClock className={styles.pendingIcon} size={22} aria-hidden="true" />
          <div><h2>正在等待元数据</h2><p>磁力链接已被接受，可以安全离开当前页面。后台服务解析元数据后，文件和数据块会显示在这里。</p></div>
        </div>
      )}

      {status.error !== null && status.error !== undefined && status.data !== undefined && (
        <div className="refresh-warning" role="alert"><IconRefresh size={16} aria-hidden="true" /> 最新状态快照刷新失败，当前显示上一次有效快照。</div>
      )}
      {status.error !== null && status.error !== undefined && status.data === undefined && !pending && (
        <Alert color="yellow" icon={<IconAlertTriangle size={16} />} title="状态快照暂不可用">{userFacingError(status.error, '暂时无法读取实时状态，请稍后重试。')}</Alert>
      )}

      <Tabs defaultValue="overview" className={styles.tabs}>
        <Tabs.List aria-label="任务详情分区">
          <Tabs.Tab value="overview">概览</Tabs.Tab>
          <Tabs.Tab value="files">文件 <span className="subtle">· {files.length}</span></Tabs.Tab>
          <Tabs.Tab value="pieces">数据块 <span className="subtle">· {pieces.length}</span></Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="overview" className={styles.tabPanel}>
          <TorrentOverview torrent={torrent} summary={liveSummary.data} meta={statusMeta.data} />
        </Tabs.Panel>
        <Tabs.Panel value="files" className={styles.tabPanel}>
          <TorrentFilesPanel files={files} pieces={pieces} subtitles={subtitles} />
        </Tabs.Panel>
        <Tabs.Panel value="pieces" className={styles.tabPanel}>
          <TorrentPiecesPanel pieces={pieces} pieceLength={statusMeta.data?.pieceLength} />
        </Tabs.Panel>
      </Tabs>

      <UploadSubtitleDialog
        api={auth.api}
        torrentId={id}
        targets={targets}
        subtitles={subtitles}
        opened={subtitleOpen}
        onClose={() => setSubtitleOpen(false)}
      />

      <DeleteTorrentDialog torrent={torrent} opened={deleteOpen} onClose={() => setDeleteOpen(false)} />
    </div>
  );
}
