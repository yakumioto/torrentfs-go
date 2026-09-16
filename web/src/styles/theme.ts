import { createTheme, rem } from '@mantine/core';

export const theme = createTheme({
  primaryColor: 'mint',
  primaryShade: 5,
  colors: {
    mint: [
      '#e8fffa',
      '#c9fff3',
      '#9af8e5',
      '#72efda',
      '#5eead4',
      '#35c6b4',
      '#1ba495',
      '#118276',
      '#0b665e',
      '#064c47',
    ],
    coral: [
      '#fff0ed',
      '#ffd8d1',
      '#ffb8aa',
      '#ff9484',
      '#ff7a67',
      '#e45c4e',
      '#c7463d',
      '#a33734',
      '#832c2e',
      '#69252a',
    ],
  },
  fontFamily: '"Avenir Next", "Segoe UI", sans-serif',
  headings: {
    fontFamily: '"Arial Black", "Avenir Next", sans-serif',
    fontWeight: '800',
  },
  fontFamilyMonospace: '"IBM Plex Mono", "SFMono-Regular", Consolas, monospace',
  defaultRadius: 'sm',
  radius: {
    sm: rem(8),
    md: rem(12),
    lg: rem(18),
  },
  shadows: {
    sm: '0 8px 24px rgba(0, 0, 0, 0.18)',
    md: '0 18px 48px rgba(0, 0, 0, 0.28)',
  },
});
