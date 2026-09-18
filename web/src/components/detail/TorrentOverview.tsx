import type { Torrent } from '../../types/api';
import type { TorrentLiveSummary as TorrentLiveSummaryData, TorrentStatusMeta } from '../../queries/hooks';
import { formatBytes, formatDate } from '../../utils/format';
import { StateBadge } from '../torrents/StateBadge';
import styles from './TorrentOverview.module.css';

export function TorrentOverview({ torrent, summary, meta }: {
  torrent: Torrent;
  summary?: TorrentLiveSummaryData;
  meta?: TorrentStatusMeta;
}) {
  return (
    <section className="panel panel--padding" aria-labelledby="overview-title">
      <div className="section-heading">
        <div>
          <p className="eyebrow">任务快照</p>
          <h2 id="overview-title">概览</h2>
        </div>
        <span className="section-heading__meta">{meta?.pieceCount.toLocaleString() ?? '—'} 个数据块</span>
      </div>
      <dl className={styles.grid}>
        <div><dt>信息哈希</dt><dd className={`text-mono ${styles.gridBreak}`}>{torrent.info_hash || '等待生成'}</dd></div>
        <div><dt>状态</dt><dd><StateBadge state={summary?.state ?? torrent.state} /></dd></div>
        <div><dt>总大小</dt><dd>{formatBytes(summary?.totalBytes ?? torrent.total_bytes)}</dd></div>
        <div><dt>缓存占用</dt><dd>{formatBytes(summary?.cachedBytes ?? torrent.cached_bytes)}</dd></div>
        <div><dt>添加时间</dt><dd>{formatDate(torrent.created_at)}</dd></div>
        <div><dt>数据块大小</dt><dd>{meta === undefined ? '—' : `${formatBytes(meta.pieceLength)}`}</dd></div>
        <div><dt>数据块数量</dt><dd>{meta?.pieceCount.toLocaleString() ?? '—'}</dd></div>
      </dl>
    </section>
  );
}
