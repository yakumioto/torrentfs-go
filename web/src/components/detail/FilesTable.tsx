import { Table } from '@mantine/core';
import type { FileStatus, PieceStatus } from '../../types/api';
import { formatBytes } from '../../utils/format';
import { fileCoverage } from './file-coverage';

export function FilesTable({ files, pieces }: { files: FileStatus[]; pieces: PieceStatus[] }) {
  return (
    <div className="files-table-wrap">
      <Table className="files-table" highlightOnHover verticalSpacing="md">
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
                <Table.Td><span className="file-path">{file.path}</span></Table.Td>
                <Table.Td><span className="text-mono">{formatBytes(file.size)}</span></Table.Td>
                <Table.Td><span className="text-mono">[{file.piece_start}, {file.piece_end})</span></Table.Td>
                <Table.Td><span className={`coverage-copy coverage-copy--${coverage.tone}`}>{coverage.label}</span></Table.Td>
              </Table.Tr>
            );
          })}
        </Table.Tbody>
      </Table>
      {files.length === 0 && <div className="empty-state"><p>No files are available in this snapshot.</p></div>}
      <p className="subtle" style={{ margin: '1rem 0 0', fontSize: '0.78rem' }}>Coverage is piece-level. A file is marked Complete only when every referenced piece is complete.</p>
    </div>
  );
}
