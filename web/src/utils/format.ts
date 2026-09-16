export function formatBytes(value: number): string {
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

export function isGoZeroTime(value: string): boolean {
  return value === '0001-01-01T00:00:00Z';
}

export function formatDate(value: string): string {
  if (isGoZeroTime(value)) {
    return '—';
  }
  const timestamp = Date.parse(value);
  if (!Number.isFinite(timestamp)) {
    return '—';
  }
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(timestamp);
}

export function percent(value: number): number {
  if (!Number.isFinite(value)) {
    return 0;
  }
  return Math.min(100, Math.max(0, value * 100));
}
