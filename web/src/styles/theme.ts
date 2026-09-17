import { createTheme, rem } from '@mantine/core';

export const theme = createTheme({
  primaryColor: 'mint',
  primaryShade: 5,
  colors: {
    mint: [
      '#ddf4ff',
      '#b6e3ff',
      '#8acbff',
      '#66b3ff',
      '#4493e6',
      '#2f81f7',
      '#1f6feb',
      '#1158c7',
      '#0d419d',
      '#0a306f',
    ],
    coral: [
      '#ffebe9',
      '#ffdcd7',
      '#ffc1ba',
      '#ffa198',
      '#f78166',
      '#f85149',
      '#da3633',
      '#b62324',
      '#8e1519',
      '#67060c',
    ],
  },
  fontFamily: 'Inter, "Segoe UI", sans-serif',
  headings: {
    fontFamily: 'Inter, "Segoe UI", sans-serif',
    fontWeight: '700',
  },
  fontFamilyMonospace: '"SFMono-Regular", Consolas, "Liberation Mono", monospace',
  defaultRadius: 'sm',
  radius: {
    sm: rem(6),
    md: rem(8),
    lg: rem(12),
  },
  shadows: {
    sm: '0 4px 12px rgba(0, 0, 0, 0.18)',
    md: '0 12px 32px rgba(0, 0, 0, 0.24)',
  },
});
