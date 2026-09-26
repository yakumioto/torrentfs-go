import { memo } from 'react';
import type { FileStatus, PieceStatus, Subtitle } from '../../types/api';
import { FilesTable } from './FilesTable';
import { SubtitlesTable } from './SubtitlesTable';

export const TorrentFilesPanel = memo(function TorrentFilesPanel({ files, pieces, subtitles }: { files: FileStatus[]; pieces: PieceStatus[]; subtitles: Subtitle[] }) {
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

      <div className="section-heading" style={{ marginTop: '1.5rem' }}>
        <div>
          <p className="eyebrow">上传的字幕</p>
          <h2 id="subtitles-title">已管理字幕</h2>
        </div>
        <span className="section-heading__meta">{subtitles.length.toLocaleString()} 个字幕</span>
      </div>
      <p className="subtle" style={{ margin: '0 0 0.75rem', fontSize: '0.78rem' }}>这些字幕由 TorrentFS 通过认证 API 写入并合并进只读挂载点，不属于种子自带文件，也不参与上面的数据块覆盖统计。</p>
      <SubtitlesTable subtitles={subtitles} />
    </section>
  );
});
