export interface Torrent {
  id: string;
  info_hash: string;
  name: string;
  state: string;
  total_bytes: number;
  cached_bytes: number;
  created_at: string;
  error?: string;
}

export interface PieceStatus {
  index: number;
  cached: boolean;
  cached_bytes: number;
  pinned: boolean;
}

export interface FileStatus {
  path: string;
  size: number;
  piece_start: number;
  piece_end: number;
}

export interface TorrentStatus {
  torrent: Torrent;
  metainfo_ready: boolean;
  piece_length: number;
  pieces: PieceStatus[];
  files: FileStatus[];
}

export interface Operation {
  operation_id: string;
  torrent_id: string;
  state: string;
  error?: string;
}

export interface LoginResponse {
  token: string;
  token_type: 'Bearer' | string;
  expires_in: number;
}
