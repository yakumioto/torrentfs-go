import { formatBytes, percent } from '../../utils/format';
import styles from './TorrentProgress.module.css';

const STATE_CLASS: Record<string, string> = {
  adding: styles.adding,
  deleting: styles.deleting,
  ready: styles.ready,
  error: styles.error,
  delete_failed: styles.deleteFailed,
};

export function TorrentProgress({ cachedBytes, totalBytes, state, className }: {
  cachedBytes: number;
  totalBytes: number;
  state?: string;
  className?: string;
}) {
  const ratio = Number.isFinite(cachedBytes) && Number.isFinite(totalBytes) && totalBytes > 0 ? cachedBytes / totalBytes : 0;
  const value = percent(ratio);
  const stateClass = state === undefined ? undefined : STATE_CLASS[state];
  return (
    <div className={[styles.root, stateClass, className].filter(Boolean).join(' ')}>
      <div className={styles.track} role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={value} aria-label={`${value.toFixed(1)}% Torrent 缓存占用`}>
        <div className={styles.fill} style={{ width: `${value}%` }} />
      </div>
      <div className={styles.label}>
        <span><span className={styles.labelTitle}>Torrent 缓存占用</span> {value.toFixed(value === 100 ? 0 : 1)}%</span>
        <span>{formatBytePair(cachedBytes, totalBytes)}</span>
      </div>
    </div>
  );
}

function formatBytePair(cachedBytes: number, totalBytes: number): string {
  return `Torrent 缓存 ${formatBytes(cachedBytes)} / Torrent 总大小 ${formatBytes(totalBytes)}`;
}
