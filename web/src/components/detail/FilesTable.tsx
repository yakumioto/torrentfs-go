import { Table } from '@mantine/core';
import type { FileStatus, PieceStatus } from '../../types/api';
import { formatBytes } from '../../utils/format';
import { fileCoverage } from './file-coverage';
import styles from './FilesTable.module.css';

const COVERAGE_CLASS: Record<string, string> = {
  cached: styles.coverageCached,
  partial: styles.coveragePartial,
  uncached: styles.coverageUncached,
};

export function FilesTable({ files, pieces }: { files: FileStatus[]; pieces: PieceStatus[] }) {
  return (
    <div className="files-table-wrap">
      <Table className={styles.table} highlightOnHover verticalSpacing="md">
        <Table.Thead>
          <Table.Tr>
            <Table.Th>路径</Table.Th>
            <Table.Th>文件大小</Table.Th>
            <Table.Th>文件涉及的数据块范围（Torrent 绝对 piece 索引）</Table.Th>
            <Table.Th>所涉 piece 当前驻留内存缓存</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {files.map((file) => {
            const coverage = fileCoverage(file, pieces);
            return (
              <Table.Tr key={`${file.path}:${file.piece_start}:${file.piece_end}`}>
                <Table.Td data-label="路径"><span className={styles.filePath}>{file.path}</span></Table.Td>
                <Table.Td data-label="文件大小"><span className="text-mono">{formatBytes(file.size)}</span></Table.Td>
                <Table.Td data-label="文件涉及的数据块范围（Torrent 绝对 piece 索引）"><span className="text-mono">[{file.piece_start}, {file.piece_end})</span></Table.Td>
                <Table.Td data-label="所涉 piece 当前驻留内存缓存"><span className={`${styles.coverage} ${COVERAGE_CLASS[coverage.tone]}`}>{coverage.label}</span></Table.Td>
              </Table.Tr>
            );
          })}
        </Table.Tbody>
      </Table>
      {files.length === 0 && <div className="empty-state" role="status"><p>当前快照没有可用的文件。</p></div>}
      <p className="subtle" style={{ margin: '1rem 0 0', fontSize: '0.78rem' }}>范围使用整个 Torrent 的绝对 piece 索引。缓存覆盖只表示该文件涉及的完整 piece 当前是否驻留内存缓存，不是文件下载百分比或播放进度；cache miss、foreground 读取和后台预取都可能填充完整 piece，未固定的数据块也可能被 LRU 淘汰，因此占用和覆盖状态会回落。</p>
    </div>
  );
}
