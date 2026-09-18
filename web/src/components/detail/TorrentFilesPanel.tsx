import { memo } from 'react';
import type { FileStatus, PieceStatus } from '../../types/api';
import { FilesTable } from './FilesTable';

export const TorrentFilesPanel = memo(function TorrentFilesPanel({ files, pieces }: { files: FileStatus[]; pieces: PieceStatus[] }) {
  return (
    <section className="panel panel--padding" aria-labelledby="files-title">
      <div className="section-heading">
        <div>
          <p className="eyebrow">数据块映射</p>
          <h2 id="files-title">文件</h2>
        </div>
        <span className="section-heading__meta">{files.length.toLocaleString()} 个文件</span>
      </div>
      <FilesTable files={files} pieces={pieces} />
    </section>
  );
});
