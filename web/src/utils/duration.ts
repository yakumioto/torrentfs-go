export type DurationError = 'empty' | 'invalid_format' | 'unknown_unit' | 'overflow';

export type DurationResult = { nanoseconds: number } | { error: DurationError };

/**
 * Nanoseconds in one unit. The order matters when matching: the longest unit
 * suffix wins, so "ms" is not read as "m" followed by "s".
 */
const DURATION_UNITS: ReadonlyArray<{ suffix: string; nanoseconds: number }> = [
  { suffix: 'ns', nanoseconds: 1 },
  { suffix: 'us', nanoseconds: 1e3 },
  { suffix: 'µs', nanoseconds: 1e3 },
  { suffix: 'μs', nanoseconds: 1e3 },
  { suffix: 'ms', nanoseconds: 1e6 },
  { suffix: 's', nanoseconds: 1e9 },
  { suffix: 'm', nanoseconds: 60 * 1e9 },
  { suffix: 'h', nanoseconds: 3600 * 1e9 },
];

const NUMBER_PATTERN = /^(?:\d+(?:\.\d*)?|\.\d+)/;
const UNIT_PATTERN = /^(ns|us|µs|μs|ms|s|m|h)/;

/**
 * Parses a Go-style duration such as "30s", "1h30m", "500ms" or "-1.5h".
 *
 * The configuration file parses durations with Go's time.ParseDuration, so the
 * page accepts the same syntax before the backend becomes the authority. The
 * value is a number of nanoseconds, which is the precision Go stores.
 */
export function parseDuration(raw: string): DurationResult {
  const value = raw.trim();
  if (value === '') {
    return { error: 'empty' };
  }
  let sign = 1;
  let body = value;
  if (body.startsWith('+') || body.startsWith('-')) {
    sign = body.startsWith('-') ? -1 : 1;
    body = body.slice(1);
  }
  if (body === '') {
    return { error: 'invalid_format' };
  }

  const units = new Map(DURATION_UNITS.map(({ suffix, nanoseconds }) => [suffix, nanoseconds]));
  let total = 0;
  let index = 0;
  while (index < body.length) {
    const number = NUMBER_PATTERN.exec(body.slice(index));
    if (number === null) {
      return { error: 'invalid_format' };
    }
    index += number[0].length;
    const unit = UNIT_PATTERN.exec(body.slice(index));
    if (unit === null) {
      // A letter that is not a known unit is a misspelt unit; anything else
      // (a space, a second sign, a dot) makes the shape invalid.
      return /^[A-Za-zµμ]/.test(body.slice(index))
        ? { error: 'unknown_unit' }
        : { error: 'invalid_format' };
    }
    index += unit[0].length;
    const amount = Number(number[0]);
    const nanoseconds = units.get(unit[0]);
    if (nanoseconds === undefined || !Number.isFinite(amount)) {
      return { error: 'unknown_unit' };
    }
    total += amount * nanoseconds;
    if (!Number.isFinite(total) || Math.abs(total) > Number.MAX_SAFE_INTEGER) {
      return { error: 'overflow' };
    }
  }
  return { nanoseconds: sign * total };
}

/** User-facing explanation for a rejected duration input. */
export function durationInputError(result: DurationResult): string {
  if ('nanoseconds' in result) {
    return '';
  }
  switch (result.error) {
    case 'empty':
      return '';
    case 'unknown_unit':
      return '时长单位仅支持 ns、us、µs、ms、s、m、h。';
    case 'overflow':
      return '时长超出可表示范围。';
    case 'invalid_format':
      return '请输入合法时长，例如 30s 或 1h30m。';
  }
}
