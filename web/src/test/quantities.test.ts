import { describe, expect, it } from 'vitest';
import {
  formatByteQuantity,
  formatUploadRate,
  parseByteQuantity,
  parseUploadRate,
} from '../utils/quantities';

function valueOf(result: ReturnType<typeof parseByteQuantity>): number {
  if (!('value' in result)) {
    throw new Error(`parse failed: ${result.error}`);
  }
  return result.value;
}

describe('byte quantities', () => {
  it.each([
    ['32MB', 32_000_000],
    ['8GB', 8_000_000_000],
    ['32MiB', 33_554_432],
    [' 10MiB ', 10 * 1024 * 1024],
  ])('parses %s exactly', (input, expected) => {
    expect(valueOf(parseByteQuantity(input))).toBe(expected);
  });

  it.each([
    ['32', 'missing_unit'],
    ['32 MB', 'invalid_format'],
    ['1.5MB', 'invalid_format'],
    ['1e3MB', 'invalid_format'],
    ['32Mb', 'unknown_unit'],
    ['32PB', 'unknown_unit'],
    ['9007199254740992B', 'overflow'],
  ])('rejects %s with %s', (input, error) => {
    expect(parseByteQuantity(input)).toEqual({ error });
  });

  it.each([1, 1_234_567, 32_000_000, 33_554_432, 8_000_000_000])('formats %d without loss', (value) => {
    const formatted = formatByteQuantity(value);
    expect(formatted).toBeDefined();
    expect(valueOf(parseByteQuantity(formatted ?? ''))).toBe(value);
  });
});

describe('upload rates', () => {
  it.each([
    ['1MiB/s', 1_048_576],
    ['32MB/s', 32_000_000],
  ])('parses %s as bytes per second', (input, expected) => {
    expect(valueOf(parseUploadRate(input))).toBe(expected);
  });

  it('requires the /s suffix', () => {
    expect(parseUploadRate('1MiB')).toEqual({ error: 'missing_rate_suffix' });
  });

  it('formats rates with an exact unit', () => {
    expect(formatUploadRate(1_048_576)).toBe('1MiB/s');
    expect(formatUploadRate(1_234_567)).toBe('1234567B/s');
  });
});
