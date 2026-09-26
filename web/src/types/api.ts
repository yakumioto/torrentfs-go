export interface Torrent {
  id: string;
  info_hash: string;
  name: string;
  state: string;
  total_bytes: number;
  downloaded_bytes: number;
  uploaded_bytes: number;
  cached_bytes: number;
  created_at: string;
  favorite: boolean;
  error?: string;
}

export interface RuntimeStats {
  started_at: string;
  cache: {
    used_bytes: number;
    capacity_bytes: number;
  };
  transfer: {
    downloaded_bytes: number;
    uploaded_bytes: number;
  };
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

export interface SubtitleTarget {
  video_path: string;
  mount_path: string;
  expected_basename: string;
  uploadable: boolean;
  reason?: string;
}

export interface Subtitle {
  video_path: string;
  path: string;
  mount_path: string;
  format: string;
  size: number;
  updated_at: string;
}

export interface SubtitleUploadResponse {
  torrent_id: string;
  video_path: string;
  path: string;
  mount_path: string;
  format: string;
  size: number;
  updated_at: string;
  replaced: boolean;
}

export interface TorrentStatus {
  torrent: Torrent;
  metainfo_ready: boolean;
  piece_length: number;
  pieces: PieceStatus[];
  files: FileStatus[];
  subtitle_targets: SubtitleTarget[];
  subtitles: Subtitle[];
}

export interface Operation {
  operation_id: string;
  torrent_id: string;
  state: string;
  error?: string;
  error_code?: string;
}

export interface PruneFailure {
  torrent_id: string;
  error: string;
}

export interface PruneResult {
  operations: Operation[];
  excluded_favorites: number;
  /** Absent when every matched torrent started deleting. */
  failures?: PruneFailure[];
}

export interface LoginResponse {
  token: string;
  token_type: 'Bearer' | string;
  expires_in: number;
}
