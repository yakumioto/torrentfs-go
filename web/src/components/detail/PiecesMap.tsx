import { Button, Table } from '@mantine/core';
import type { PieceStatus } from '../../types/api';
import { formatBytes } from '../../utils/format';
import { useState } from 'react';
import type { PieceVisualState } from './piece-state';
import { pieceVisualState } from './piece-state';
import styles from './PiecesMap.module.css';

const PAGE_SIZE = 1200;

const CELL_CLASS: Record<PieceVisualState, string> = {
  complete: styles.cellComplete,
  partial: styles.cellPartial,
  incomplete: styles.cellIncomplete,
  checking: styles.cellChecking,
  unknown: styles.cellUnknown,
};

const SWATCH_CLASS: Record<PieceVisualState, string> = {
  complete: styles.swatchComplete,
  partial: styles.swatchPartial,
  incomplete: styles.swatchIncomplete,
  checking: styles.swatchChecking,
  unknown: styles.swatchUnknown,
};

export function PiecesMap({ pieces }: { pieces: PieceStatus[] }) {
  const [visibleCount, setVisibleCount] = useState(PAGE_SIZE);
  const visiblePieces = pieces.slice(0, visibleCount);

  return (
    <div>
      <div className={styles.toolbar}>
        <p className={styles.toolbarCopy}>Each cell is one absolute piece. A white ring means the piece is wanted; texture keeps states readable without color.</p>
        <span className="text-mono">{pieces.length.toLocaleString()} pieces</span>
      </div>
      <div className={styles.legend} aria-label="Piece state legend">
        <LegendItem state="complete" label="Complete" />
        <LegendItem state="partial" label="Partial" />
        <LegendItem state="incomplete" label="Known, incomplete" />
        <LegendItem state="checking" label="Checking" />
        <LegendItem state="unknown" label="Unknown" />
      </div>
      <div className={styles.map} role="list" aria-label="Piece map">
        {visiblePieces.length === 0 && <div className={styles.mapEmpty}>No piece metadata is available yet.</div>}
        {visiblePieces.map((piece) => {
          const state = pieceVisualState(piece);
          return (
            <span
              className={`${styles.cell} ${CELL_CLASS[state]}${piece.wanted ? ` ${styles.cellWanted}` : ''}`}
              key={piece.index}
              role="listitem"
              title={pieceTooltip(piece, state)}
              aria-label={pieceTooltip(piece, state)}
            />
          );
        })}
      </div>
      {visibleCount < pieces.length && (
        <div className={styles.mapMore}><Button variant="subtle" color="mint" onClick={() => setVisibleCount((count) => count + PAGE_SIZE)}>Show next {Math.min(PAGE_SIZE, pieces.length - visibleCount).toLocaleString()} pieces</Button></div>
      )}
      <details style={{ marginTop: '1rem' }}>
        <summary className="subtle">Open accessible piece table</summary>
        <div className="files-table-wrap" style={{ marginTop: '0.75rem', maxHeight: '20rem', overflow: 'auto' }}>
          <Table striped withTableBorder className={styles.table}>
            <Table.Thead><Table.Tr><Table.Th>Index</Table.Th><Table.Th>State</Table.Th><Table.Th>Wanted</Table.Th><Table.Th>Available</Table.Th></Table.Tr></Table.Thead>
            <Table.Tbody>
              {visiblePieces.map((piece) => <Table.Tr key={`table-${piece.index}`}><Table.Td><span className="text-mono">{piece.index}</span></Table.Td><Table.Td>{pieceVisualState(piece)}</Table.Td><Table.Td>{piece.wanted ? 'Yes' : 'No'}</Table.Td><Table.Td>{piece.available_bytes === undefined ? '—' : formatBytes(piece.available_bytes)}</Table.Td></Table.Tr>)}
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
  const details = [`Piece ${piece.index}`, stateLabel(state), piece.wanted ? 'wanted' : 'not wanted'];
  if (piece.available_bytes !== undefined) {
    details.push(`${formatBytes(piece.available_bytes)} available`);
  }
  return details.join(' · ');
}

function stateLabel(state: PieceVisualState): string {
  return state === 'incomplete' ? 'known, incomplete' : state;
}
