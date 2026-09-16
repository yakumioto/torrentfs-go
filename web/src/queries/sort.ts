import type { Torrent } from '../types/api';
import { isGoZeroTime } from '../utils/format';

export function sortTorrents(torrents: Torrent[]): Torrent[] {
  return [...torrents].sort((left, right) => {
    const nameOrder = left.name.localeCompare(right.name, undefined, { sensitivity: 'base' });
    if (nameOrder !== 0) {
      return nameOrder;
    }

    const leftCreated = Date.parse(left.created_at);
    const rightCreated = Date.parse(right.created_at);
    const leftHasDate = !isGoZeroTime(left.created_at) && Number.isFinite(leftCreated);
    const rightHasDate = !isGoZeroTime(right.created_at) && Number.isFinite(rightCreated);
    if (leftHasDate !== rightHasDate) {
      return leftHasDate ? -1 : 1;
    }
    if (leftCreated !== rightCreated) {
      return rightCreated - leftCreated;
    }
    return left.id.localeCompare(right.id);
  });
}
