import { Table } from '@mantine/core';
import type { Subtitle } from '../../types/api';
import { formatBytes } from '../../utils/format';
import styles from './FilesTable.module.css';

/**
 * Managed subtitles are listed separately from the payload table: they carry no
 * piece range, so folding them into the piece-coverage view would report a
 * coverage state that does not exist.
 */
export function SubtitlesTable({ subtitles }: { subtitles: Subtitle[] }) {
  if (subtitles.length === 0) {
    return <div className="empty-state" role="status"><p>还没有由 TorrentFS 管理的字幕。</p></div>;
  }
  return (
    <div className="files-table-wrap">
      <Table className={styles.table} highlightOnHover verticalSpacing="md">
        <Table.Thead>
          <Table.Tr>
            <Table.Th>目标视频</Table.Th>
            <Table.Th>挂载路径</Table.Th>
            <Table.Th>格式</Table.Th>
            <Table.Th>大小</Table.Th>
            <Table.Th>更新时间</Table.Th>
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {subtitles.map((subtitle) => (
            <Table.Tr key={subtitle.path}>
              <Table.Td data-label="目标视频"><span className={styles.filePath}>{subtitle.video_path}</span></Table.Td>
              <Table.Td data-label="挂载路径"><span className={`${styles.filePath} text-mono`}>{subtitle.mount_path}</span></Table.Td>
              <Table.Td data-label="格式"><span className="text-mono">.{subtitle.format}</span></Table.Td>
              <Table.Td data-label="大小"><span className="text-mono">{formatBytes(subtitle.size)}</span></Table.Td>
              <Table.Td data-label="更新时间"><span className="text-mono">{new Date(subtitle.updated_at).toLocaleString()}</span></Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </div>
  );
}
