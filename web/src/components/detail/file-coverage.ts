import type { FileStatus, PieceStatus } from '../../types/api';

export interface Coverage {
  label: string;
  tone: 'complete' | 'partial' | 'waiting';
}

export function fileCoverage(file: FileStatus, pieces: PieceStatus[]): Coverage {
  if (file.piece_start === file.piece_end) {
    return { label: 'Complete · zero bytes', tone: 'complete' };
  }
  const range = pieces.slice(Math.max(0, file.piece_start), Math.max(file.piece_start, file.piece_end));
  if (range.length === 0) {
    return { label: 'Waiting for pieces', tone: 'waiting' };
  }
  const complete = range.filter((piece) => piece.complete).length;
  const partial = range.filter((piece) => piece.partial && !piece.complete).length;
  if (complete === range.length) {
    return { label: 'Complete', tone: 'complete' };
  }
  if (complete > 0 || partial > 0) {
    return { label: `${complete} complete · ${partial} partial`, tone: 'partial' };
  }
  return { label: `${range.length} pieces waiting`, tone: 'waiting' };
}
