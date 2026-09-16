import { Button, Table } from '@mantine/core';
import type { PieceStatus } from '../../types/api';
import { formatBytes } from '../../utils/format';
import { useState } from 'react';
import type { PieceVisualState } from './piece-state';
import { pieceVisualState } from './piece-state';

const PAGE_SIZE = 1200;

export function PiecesMap({ pieces }: { pieces: PieceStatus[] }) {
  const [visibleCount, setVisibleCount] = useState(PAGE_SIZE);
  const visiblePieces = pieces.slice(0, visibleCount);

  return (
    <div>
      <div className="piece-toolbar">
        <p className="piece-toolbar__copy">Each cell is one absolute piece. A white ring means the piece is wanted; texture keeps states readable without color.</p>
        <span className="text-mono">{pieces.length.toLocaleString()} pieces</span>
      </div>
      <div className="piece-legend" aria-label="Piece state legend">
        <LegendItem state="complete" label="Complete" />
        <LegendItem state="partial" label="Partial" />
        <LegendItem state="incomplete" label="Known, incomplete" />
        <LegendItem state="checking" label="Checking" />
        <LegendItem state="unknown" label="Unknown" />
      </div>
      <div className="piece-map" role="list" aria-label="Piece map">
        {visiblePieces.length === 0 && <div className="piece-map__empty">No piece metadata is available yet.</div>}
        {visiblePieces.map((piece) => {
          const state = pieceVisualState(piece);
          const wanted = piece.wanted ? ' piece-cell--wanted' : '';
          return (
            <span
              className={`piece-cell piece-cell--${state}${wanted}`}
              key={piece.index}
              role="listitem"
              title={pieceTooltip(piece, state)}
              aria-label={pieceTooltip(piece, state)}
            />
          );
        })}
      </div>
      {visibleCount < pieces.length && (
        <div className="piece-map__more"><Button variant="subtle" color="mint" onClick={() => setVisibleCount((count) => count + PAGE_SIZE)}>Show next {Math.min(PAGE_SIZE, pieces.length - visibleCount).toLocaleString()} pieces</Button></div>
      )}
      <details style={{ marginTop: '1rem' }}>
        <summary className="subtle">Open accessible piece table</summary>
        <div className="files-table-wrap" style={{ marginTop: '0.75rem', maxHeight: '20rem', overflow: 'auto' }}>
          <Table striped withTableBorder className="files-table">
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
  return <span className="legend-item"><span className={`legend-swatch legend-swatch--${state}`} aria-hidden="true" />{label}</span>;
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
