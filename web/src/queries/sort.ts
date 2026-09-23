import type { Torrent } from '../types/api';
import { isGoZeroTime } from '../utils/format';

export type TorrentSortKey = 'name' | 'total_bytes' | 'state' | 'created_at';
export type SortDirection = 'asc' | 'desc';

export interface TorrentSort {
  key: TorrentSortKey;
  direction: SortDirection;
}

export const DEFAULT_TORRENT_SORT: TorrentSort = { key: 'created_at', direction: 'desc' };

const DEFAULT_SORT_DIRECTIONS: Record<TorrentSortKey, SortDirection> = {
  name: 'asc',
  total_bytes: 'desc',
  state: 'asc',
  created_at: 'desc',
};

const STATE_RANK: Record<string, number> = {
  adding: 0,
  ready: 1,
  deleting: 2,
  error: 3,
  delete_failed: 4,
};

const collator = new Intl.Collator('zh-CN', { sensitivity: 'base', numeric: true });

export function defaultSortDirection(key: TorrentSortKey): SortDirection {
  return DEFAULT_SORT_DIRECTIONS[key];
}

export function sortTorrents(torrents: Torrent[], sort: TorrentSort = DEFAULT_TORRENT_SORT): Torrent[] {
  return [...torrents].sort((left, right) => compareTorrents(left, right, sort));
}

function compareTorrents(left: Torrent, right: Torrent, sort: TorrentSort): number {
  switch (sort.key) {
    case 'name':
      return compareName(left, right);
    case 'total_bytes':
      return compareNumber(left.total_bytes, right.total_bytes, sort.direction, left, right);
    case 'state':
      return compareState(left, right, sort.direction);
    case 'created_at':
      return compareDate(left, right, sort.direction);
  }
}

function compareName(left: Torrent, right: Torrent): number {
  const nameOrder = compareTextWithEmptyLast(left.name, right.name);
  if (nameOrder !== 0) {
    return nameOrder;
  }
  const hashOrder = collator.compare(left.info_hash, right.info_hash);
  return hashOrder !== 0 ? hashOrder : collator.compare(left.id, right.id);
}

function compareNameAndID(left: Torrent, right: Torrent): number {
  const nameOrder = compareTextWithEmptyLast(left.name, right.name);
  return nameOrder !== 0 ? nameOrder : collator.compare(left.id, right.id);
}

function compareTextWithEmptyLast(left: string, right: string): number {
  const leftValue = left.trim();
  const rightValue = right.trim();
  if (leftValue === '' || rightValue === '') {
    if (leftValue === rightValue) {
      return 0;
    }
    return leftValue === '' ? 1 : -1;
  }
  return collator.compare(leftValue, rightValue);
}

function compareNumber(leftValue: number, rightValue: number, direction: SortDirection, left: Torrent, right: Torrent): number {
  const leftValid = Number.isFinite(leftValue) && leftValue >= 0;
  const rightValid = Number.isFinite(rightValue) && rightValue >= 0;
  if (leftValid !== rightValid) {
    return leftValid ? -1 : 1;
  }
  if (leftValid && leftValue !== rightValue) {
    return (leftValue - rightValue) * directionSign(direction);
  }
  return compareNameAndID(left, right);
}

function compareState(left: Torrent, right: Torrent, direction: SortDirection): number {
  const leftRank = STATE_RANK[left.state];
  const rightRank = STATE_RANK[right.state];
  const leftKnown = leftRank !== undefined;
  const rightKnown = rightRank !== undefined;
  if (leftKnown !== rightKnown) {
    return leftKnown ? -1 : 1;
  }
  if (leftKnown && rightKnown && leftRank !== rightRank) {
    return (leftRank - rightRank) * directionSign(direction);
  }
  if (!leftKnown && !rightKnown) {
    const rawStateOrder = collator.compare(left.state, right.state);
    if (rawStateOrder !== 0) {
      return rawStateOrder;
    }
  }
  return compareNameAndID(left, right);
}

function compareDate(left: Torrent, right: Torrent, direction: SortDirection): number {
  const leftValue = Date.parse(left.created_at);
  const rightValue = Date.parse(right.created_at);
  const leftValid = !isGoZeroTime(left.created_at) && Number.isFinite(leftValue);
  const rightValid = !isGoZeroTime(right.created_at) && Number.isFinite(rightValue);
  if (leftValid !== rightValid) {
    return leftValid ? -1 : 1;
  }
  if (leftValid && leftValue !== rightValue) {
    return (leftValue - rightValue) * directionSign(direction);
  }
  return compareNameAndID(left, right);
}

function directionSign(direction: SortDirection): number {
  return direction === 'asc' ? 1 : -1;
}
