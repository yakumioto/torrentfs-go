import { percent } from '../../utils/format';
import styles from './TorrentProgress.module.css';

const STATE_CLASS: Record<string, string> = {
  adding: styles.adding,
  deleting: styles.deleting,
  seeding: styles.seeding,
  error: styles.error,
  delete_failed: styles.deleteFailed,
};

export function TorrentProgress({ progress, completedBytes, totalBytes, state, className }: {
  progress: number;
  completedBytes: number;
  totalBytes: number;
  state?: string;
  className?: string;
}) {
  const value = percent(progress);
  const stateClass = state === undefined ? undefined : STATE_CLASS[state];
  return (
    <div className={[styles.root, stateClass, className].filter(Boolean).join(' ')}>
      <div className={styles.track} role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={value} aria-label={`${value.toFixed(1)} percent complete`}>
        <div className={styles.fill} style={{ width: `${value}%` }} />
      </div>
      <div className={styles.label}>
        <span>{value.toFixed(value === 100 ? 0 : 1)}%</span>
        <span>{formatBytePair(completedBytes, totalBytes)}</span>
      </div>
    </div>
  );
}

function formatBytePair(completedBytes: number, totalBytes: number): string {
  return `${compactBytes(completedBytes)} / ${compactBytes(totalBytes)}`;
}

function compactBytes(value: number): string {
  if (!Number.isFinite(value) || value < 0) {
    return '—';
  }
  if (value < 1024) {
    return `${value} B`;
  }
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let amount = value;
  let unit = 'B';
  for (const nextUnit of units) {
    amount /= 1024;
    unit = nextUnit;
    if (amount < 1024 || nextUnit === units[units.length - 1]) {
      break;
    }
  }
  return `${amount.toFixed(amount >= 10 ? 0 : 1)} ${unit}`;
}
