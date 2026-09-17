import { memo } from 'react';
import type { FileStatus, PieceStatus } from '../../types/api';
import { FilesTable } from './FilesTable';

export const TorrentFilesPanel = memo(function TorrentFilesPanel({ files, pieces }: { files: FileStatus[]; pieces: PieceStatus[] }) {
  return (
    <section className="panel panel--padding" aria-labelledby="files-title">
      <div className="section-heading">
        <div>
          <p className="eyebrow">Piece projection</p>
          <h2 id="files-title">Files</h2>
        </div>
        <span className="section-heading__meta">{files.length.toLocaleString()} files</span>
      </div>
      <FilesTable files={files} pieces={pieces} />
    </section>
  );
});
