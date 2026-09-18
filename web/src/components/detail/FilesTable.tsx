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
            <Table.Th>大小</Table.Th>
            <Table.Th>数据块范围</Table.Th>
            <Table.Th>缓存覆盖</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {files.map((file) => {
            const coverage = fileCoverage(file, pieces);
            return (
              <Table.Tr key={`${file.path}:${file.piece_start}:${file.piece_end}`}>
                <Table.Td data-label="路径"><span className={styles.filePath}>{file.path}</span></Table.Td>
                <Table.Td data-label="大小"><span className="text-mono">{formatBytes(file.size)}</span></Table.Td>
                <Table.Td data-label="数据块范围"><span className="text-mono">[{file.piece_start}, {file.piece_end})</span></Table.Td>
                <Table.Td data-label="缓存覆盖"><span className={`${styles.coverage} ${COVERAGE_CLASS[coverage.tone]}`}>{coverage.label}</span></Table.Td>
              </Table.Tr>
            );
          })}
        </Table.Tbody>
      </Table>
      {files.length === 0 && <div className="empty-state" role="status"><p>当前快照没有可用的文件。</p></div>}
      <p className="subtle" style={{ margin: '1rem 0 0', fontSize: '0.78rem' }}>缓存覆盖反映当前内存缓存状态。只有引用的每个数据块都在缓存中，文件才会显示为完全已缓存；缓存可能被淘汰，因此覆盖状态会回落。</p>
    </div>
  );
}
