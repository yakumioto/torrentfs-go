import { percent } from '../../utils/format';

export function TorrentProgress({ progress, completedBytes, totalBytes }: { progress: number; completedBytes: number; totalBytes: number }) {
  const value = percent(progress);
  return (
    <div className="torrent-row__progress">
      <div className="progress-track" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={value} aria-label={`${value.toFixed(1)} percent complete`}>
        <div className="progress-track__fill" style={{ width: `${value}%` }} />
      </div>
      <div className="progress-track__label">
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
