import type { FileStatus, PieceStatus } from '../../types/api';

export interface Coverage {
  label: string;
  tone: 'cached' | 'partial' | 'uncached';
}

export function fileCoverage(file: FileStatus, pieces: PieceStatus[]): Coverage {
  if (file.piece_start === file.piece_end) {
    return { label: 'Zero bytes', tone: 'cached' };
  }
  const range = pieces.slice(Math.max(0, file.piece_start), Math.max(file.piece_start, file.piece_end));
  if (range.length === 0) {
    return { label: 'No piece metadata', tone: 'uncached' };
  }
  const cached = range.filter((piece) => piece.cached).length;
  if (cached === range.length) {
    return { label: 'Cached', tone: 'cached' };
  }
  if (cached > 0) {
    return { label: `Cached ${cached} / ${range.length} pieces`, tone: 'partial' };
  }
  return { label: `Cached 0 / ${range.length} pieces`, tone: 'uncached' };
}
