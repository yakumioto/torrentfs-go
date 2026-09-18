import { Button, Table } from '@mantine/core';
import type { PieceStatus } from '../../types/api';
import { formatBytes } from '../../utils/format';
import { useState } from 'react';
import type { PieceVisualState } from './piece-state';
import { pieceVisualState } from './piece-state';
import styles from './PiecesMap.module.css';

const PAGE_SIZE = 1200;

const CELL_CLASS: Record<PieceVisualState, string> = {
  cached: styles.cellCached,
  pinned: styles.cellPinned,
  uncached: styles.cellUncached,
};

const SWATCH_CLASS: Record<PieceVisualState, string> = {
  cached: styles.swatchCached,
  pinned: styles.swatchPinned,
  uncached: styles.swatchUncached,
};

export function PiecesMap({ pieces }: { pieces: PieceStatus[] }) {
  const [visibleCount, setVisibleCount] = useState(PAGE_SIZE);
  const visiblePieces = pieces.slice(0, visibleCount);

  return (
    <div>
      <div className={styles.toolbar}>
        <p className={styles.toolbarCopy}>每个方块代表一个绝对数据块。已固定的数据块会留在缓存中并避免被淘汰；纹理让状态不依赖颜色也能区分。</p>
        <span className="text-mono">{pieces.length.toLocaleString()} 个数据块</span>
      </div>
      <div className={styles.legend} aria-label="数据块状态图例">
        <LegendItem state="cached" label="已缓存" />
        <LegendItem state="pinned" label="已固定" />
        <LegendItem state="uncached" label="未缓存" />
      </div>
      <div className={styles.map} role="list" aria-label="数据块地图">
        {visiblePieces.length === 0 && <div className={styles.mapEmpty}>当前还没有可用的数据块元数据。</div>}
        {visiblePieces.map((piece) => {
          const state = pieceVisualState(piece);
          return (
            <span
              className={`${styles.cell} ${CELL_CLASS[state]}`}
              key={piece.index}
              role="listitem"
              title={pieceTooltip(piece, state)}
              aria-label={pieceTooltip(piece, state)}
            />
          );
        })}
      </div>
      {visibleCount < pieces.length && (
        <div className={styles.mapMore}><Button variant="subtle" color="torrent" onClick={() => setVisibleCount((count) => count + PAGE_SIZE)}>继续查看后面 {Math.min(PAGE_SIZE, pieces.length - visibleCount).toLocaleString()} 个数据块</Button></div>
      )}
      <details className={styles.details}>
        <summary>打开可访问的数据块明细表</summary>
        <div className="files-table-wrap" style={{ marginTop: '0.75rem', maxHeight: '20rem', overflow: 'auto' }}>
          <Table striped withTableBorder className={styles.table}>
            <Table.Thead><Table.Tr><Table.Th>索引</Table.Th><Table.Th>状态</Table.Th><Table.Th>缓存大小</Table.Th><Table.Th>是否固定</Table.Th></Table.Tr></Table.Thead>
            <Table.Tbody>
              {visiblePieces.map((piece) => <Table.Tr key={`table-${piece.index}`}><Table.Td data-label="索引"><span className="text-mono">{piece.index}</span></Table.Td><Table.Td data-label="状态">{pieceVisualStateLabel(piece)}</Table.Td><Table.Td data-label="缓存大小">{formatBytes(piece.cached_bytes)}</Table.Td><Table.Td data-label="是否固定">{piece.pinned ? '是' : '否'}</Table.Td></Table.Tr>)}
            </Table.Tbody>
          </Table>
        </div>
      </details>
    </div>
  );
}

function LegendItem({ state, label }: { state: PieceVisualState; label: string }) {
  return <span className={styles.legendItem}><span className={`${styles.swatch} ${SWATCH_CLASS[state]}`} aria-hidden="true" />{label}</span>;
}

function pieceTooltip(piece: PieceStatus, state: PieceVisualState): string {
  return [`数据块 ${piece.index}`, pieceVisualStateLabelFromState(state), `${formatBytes(piece.cached_bytes)} 已缓存`].join(' · ');
}

function pieceVisualStateLabel(piece: PieceStatus): string {
  return pieceVisualStateLabelFromState(pieceVisualState(piece));
}

function pieceVisualStateLabelFromState(state: PieceVisualState): string {
  if (state === 'pinned') {
    return '已固定';
  }
  if (state === 'cached') {
    return '已缓存';
  }
  return '未缓存';
}
