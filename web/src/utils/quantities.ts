export type QuantityError = 'empty' | 'missing_unit' | 'invalid_format' | 'unknown_unit' | 'overflow' | 'missing_rate_suffix';

export type QuantityResult = { value: number } | { error: QuantityError };

type ByteUnit = {
  suffix: string;
  multiplier: number;
};

const BYTE_UNITS: readonly ByteUnit[] = [
  { suffix: 'TiB', multiplier: 2 ** 40 },
  { suffix: 'TB', multiplier: 10 ** 12 },
  { suffix: 'GiB', multiplier: 2 ** 30 },
  { suffix: 'GB', multiplier: 10 ** 9 },
  { suffix: 'MiB', multiplier: 2 ** 20 },
  { suffix: 'MB', multiplier: 10 ** 6 },
  { suffix: 'KiB', multiplier: 2 ** 10 },
  { suffix: 'KB', multiplier: 10 ** 3 },
  { suffix: 'B', multiplier: 1 },
];

const BYTE_UNIT_MULTIPLIERS = new Map(BYTE_UNITS.map(({ suffix, multiplier }) => [suffix, multiplier]));
const MAX_SAFE_BIGINT = BigInt(Number.MAX_SAFE_INTEGER);

function isAsciiLetters(value: string): boolean {
  return /^[A-Za-z]+$/.test(value);
}

export function parseByteQuantity(raw: string): QuantityResult {
  const value = raw.trim();
  if (value === '') {
    return { error: 'empty' };
  }

  let digitEnd = 0;
  while (digitEnd < value.length && value[digitEnd] >= '0' && value[digitEnd] <= '9') {
    digitEnd += 1;
  }
  if (digitEnd === 0) {
    return { error: 'invalid_format' };
  }
  if (digitEnd === value.length) {
    return { error: 'missing_unit' };
  }

  const suffix = value.slice(digitEnd);
  if (/\s/.test(suffix) || !isAsciiLetters(suffix)) {
    return { error: 'invalid_format' };
  }
  const multiplier = BYTE_UNIT_MULTIPLIERS.get(suffix);
  if (multiplier === undefined) {
    return { error: 'unknown_unit' };
  }

  const bytes = BigInt(value.slice(0, digitEnd)) * BigInt(multiplier);
  if (bytes > MAX_SAFE_BIGINT) {
    return { error: 'overflow' };
  }
  return { value: Number(bytes) };
}

export function parseUploadRate(raw: string): QuantityResult {
  const value = raw.trim();
  if (!value.endsWith('/s')) {
    return { error: 'missing_rate_suffix' };
  }
  const quantity = value.slice(0, -2);
  if (quantity.trim() !== quantity) {
    return { error: 'invalid_format' };
  }
  return parseByteQuantity(quantity);
}

export function formatByteQuantity(value: number): string | undefined {
  if (!Number.isSafeInteger(value) || value < 0) {
    return undefined;
  }
  for (const unit of BYTE_UNITS) {
    if (value >= unit.multiplier && value % unit.multiplier === 0) {
      return `${value / unit.multiplier}${unit.suffix}`;
    }
  }
  return `${value}B`;
}

export function formatUploadRate(value: number): string | undefined {
  const quantity = formatByteQuantity(value);
  return quantity === undefined ? undefined : `${quantity}/s`;
}
