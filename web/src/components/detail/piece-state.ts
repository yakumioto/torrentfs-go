import type { PieceStatus } from '../../types/api';

export type PieceVisualState = 'checking' | 'complete' | 'partial' | 'incomplete' | 'unknown';

export function pieceVisualState(piece: PieceStatus): PieceVisualState {
  if (piece.checking) {
    return 'checking';
  }
  if (piece.complete) {
    return 'complete';
  }
  if (piece.partial) {
    return 'partial';
  }
  if (!piece.known) {
    return 'unknown';
  }
  return 'incomplete';
}
