import { memo } from 'react';
import type { PieceStatus } from '../../types/api';
import { PiecesMap } from './PiecesMap';

export const TorrentPiecesPanel = memo(function TorrentPiecesPanel({ pieces, pieceLength }: { pieces: PieceStatus[]; pieceLength?: number }) {
  return (
    <section className="panel panel--padding" aria-labelledby="pieces-title">
      <div className="section-heading">
        <div>
          <p className="eyebrow">整个任务</p>
          <h2 id="pieces-title">数据块</h2>
        </div>
        <span className="section-heading__meta">{pieceLength === undefined ? '数据块大小等待中' : `每块 ${pieceLength.toLocaleString()} B`}</span>
      </div>
      <PiecesMap pieces={pieces} />
    </section>
  );
});
