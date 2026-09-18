import { describe, expect, it } from 'vitest';
import tokens from '../styles/tokens.css?raw';

function token(name: string): string {
  const match = tokens.match(new RegExp(`--${name}:\\s*(#[0-9a-fA-F]{6})`));
  if (match === null) {
    throw new Error(`--${name} is missing or is not a hex colour`);
  }
  return match[1].toLowerCase();
}

function relativeLuminance(hex: string): number {
  const channels = [1, 3, 5].map((offset) => parseInt(hex.slice(offset, offset + 2), 16) / 255);
  const linear = channels.map((channel) => (channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4));
  return 0.2126 * linear[0] + 0.7152 * linear[1] + 0.0722 * linear[2];
}

function contrast(foreground: string, background: string): number {
  const [lighter, darker] = [relativeLuminance(foreground), relativeLuminance(background)].sort((a, b) => b - a);
  return (lighter + 0.05) / (darker + 0.05);
}

const TEXT_TOKENS = ['text-primary', 'text-secondary', 'text-subtle'];
// --surface-3 backs the cache-occupancy track and piece cells and carries no text, so it is not a text background.
const TEXT_BACKGROUNDS = ['bg', 'surface-1', 'surface-2'];

describe('dark theme text contrast', () => {
  it.each(TEXT_TOKENS)('keeps %s at or above the 4.5:1 small-text threshold on every text surface', (name) => {
    for (const background of TEXT_BACKGROUNDS) {
      expect(contrast(token(name), token(background)), `${name} on ${background}`).toBeGreaterThanOrEqual(4.5);
    }
  });
});
