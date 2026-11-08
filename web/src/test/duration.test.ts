import { describe, expect, it } from 'vitest';
import { parseDuration } from '../utils/duration';

function valueOf(result: ReturnType<typeof parseDuration>): number | undefined {
  return 'nanoseconds' in result ? result.nanoseconds : undefined;
}

describe('parseDuration', () => {
  it.each([
    ['30s', 30 * 1e9],
    ['1h30m', 90 * 60 * 1e9],
    ['500ms', 500 * 1e6],
    ['1.5h', 90 * 60 * 1e9],
    ['.5s', 0.5 * 1e9],
    ['2h45m30s', (2 * 3600 + 45 * 60 + 30) * 1e9],
    ['-5s', -5 * 1e9],
    ['+2m', 120 * 1e9],
    ['1500us', 1500 * 1e3],
    ['1500µs', 1500 * 1e3],
  ])('parses %s', (input, expected) => {
    expect(valueOf(parseDuration(input))).toBe(expected);
  });

  it.each([
    ['', 'empty'],
    ['10', 'invalid_format'],
    ['5x', 'unknown_unit'],
    ['1h 30m', 'invalid_format'],
    ['m', 'invalid_format'],
    ['1..5s', 'invalid_format'],
    ['--1s', 'invalid_format'],
    ['1e3s', 'unknown_unit'],
    ['999999999999999999999h', 'overflow'],
  ])('rejects %s with %s', (input, error) => {
    expect(parseDuration(input)).toEqual({ error });
  });
});
