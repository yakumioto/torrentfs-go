import { memo } from 'react';
import type { PieceStatus } from '../../types/api';
import { PiecesMap } from './PiecesMap';

export const TorrentPiecesPanel = memo(function TorrentPiecesPanel({ pieces, pieceLength }: { pieces: PieceStatus[]; pieceLength?: number }) {
  return (
    <section className="panel panel--padding" aria-labelledby="pieces-title">
      <div className="section-heading">
        <div>
          <p className="eyebrow">Whole torrent</p>
          <h2 id="pieces-title">Pieces</h2>
        </div>
        <span className="section-heading__meta">{pieceLength === undefined ? 'Piece size pending' : `${pieceLength.toLocaleString()} B each`}</span>
      </div>
      <PiecesMap pieces={pieces} />
    </section>
  );
});
