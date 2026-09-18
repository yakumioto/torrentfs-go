import type { FileStatus, PieceStatus } from '../../types/api';

export interface Coverage {
  label: string;
  tone: 'cached' | 'partial' | 'uncached';
}

export function fileCoverage(file: FileStatus, pieces: PieceStatus[]): Coverage {
  if (file.piece_start === file.piece_end) {
    return { label: '零字节文件', tone: 'cached' };
  }
  const range = pieces.slice(Math.max(0, file.piece_start), Math.max(file.piece_start, file.piece_end));
  if (range.length === 0) {
    return { label: '无数据块元数据', tone: 'uncached' };
  }
  const cached = range.filter((piece) => piece.cached).length;
  if (cached === range.length) {
    return { label: '已缓存', tone: 'cached' };
  }
  if (cached > 0) {
    return { label: `已缓存 ${cached} / ${range.length} 个数据块`, tone: 'partial' };
  }
  return { label: `已缓存 0 / ${range.length} 个数据块`, tone: 'uncached' };
}
