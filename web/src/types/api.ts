export interface Torrent {
  id: string;
  info_hash: string;
  name: string;
  state: string;
  total_bytes: number;
  completed_bytes: number;
  progress: number;
  created_at: string;
  error?: string;
}

export interface PieceStatus {
  index: number;
  known: boolean;
  complete: boolean;
  partial: boolean;
  wanted: boolean;
  checking: boolean;
  available_bytes?: number;
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
  purge_data: boolean;
  error?: string;
}

export interface LoginResponse {
  token: string;
  token_type: 'Bearer' | string;
  expires_in: number;
}
