import type { PieceStatus } from '../../types/api';

export type PieceVisualState = 'cached' | 'pinned' | 'uncached';

export function pieceVisualState(piece: PieceStatus): PieceVisualState {
  if (piece.pinned) {
    return 'pinned';
  }
  if (piece.cached) {
    return 'cached';
  }
  return 'uncached';
}
