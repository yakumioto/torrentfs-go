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

/**
 * User-facing explanation for a rejected upload-rate input. The configuration page
 * uses it to phrase parser failures consistently.
 */
export function uploadRateInputError(result: QuantityResult): string {
  if ('value' in result) {
    return result.value > 0 ? '' : '请输入大于 0 的速率。';
  }
  switch (result.error) {
    case 'missing_rate_suffix':
      return '请输入带单位的速率，例如 1MiB/s。';
    case 'missing_unit':
      return '速率必须包含字节单位，例如 32MB/s。';
    case 'unknown_unit':
      return '仅支持 B、KB、MB、GB、TB、KiB、MiB、GiB 或 TiB。';
    case 'overflow':
      return '速率超出浏览器可精确编辑的范围。';
    case 'empty':
    case 'invalid_format':
      return '请输入整数速率，例如 1MiB/s；不支持小数或科学计数法。';
  }
}
