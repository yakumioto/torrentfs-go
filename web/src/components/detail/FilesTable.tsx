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
            <Table.Th>Path</Table.Th>
            <Table.Th>Size</Table.Th>
            <Table.Th>Piece range</Table.Th>
            <Table.Th>Coverage</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {files.map((file) => {
            const coverage = fileCoverage(file, pieces);
            return (
              <Table.Tr key={`${file.path}:${file.piece_start}:${file.piece_end}`}>
                <Table.Td><span className={styles.filePath}>{file.path}</span></Table.Td>
                <Table.Td><span className="text-mono">{formatBytes(file.size)}</span></Table.Td>
                <Table.Td><span className="text-mono">[{file.piece_start}, {file.piece_end})</span></Table.Td>
                <Table.Td><span className={`${styles.coverage} ${COVERAGE_CLASS[coverage.tone]}`}>{coverage.label}</span></Table.Td>
              </Table.Tr>
            );
          })}
        </Table.Tbody>
      </Table>
      {files.length === 0 && <div className="empty-state"><p>No files are available in this snapshot.</p></div>}
      <p className="subtle" style={{ margin: '1rem 0 0', fontSize: '0.78rem' }}>Coverage reflects the in-memory cache. A file is fully Cached only when every referenced piece is currently cached. Cached bytes can be evicted, so coverage may drop over time.</p>
    </div>
  );
}
