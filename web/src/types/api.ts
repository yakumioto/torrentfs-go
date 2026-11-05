export interface Category {
  name: string;
  created_at: string;
}

export interface Torrent {
  id: string;
  info_hash: string;
  name: string;
  category: string;
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

/**
 * One same-day upload window in the daemon's local time, minute precision and
 * half-open: start is included and end is not.
 */
export interface UploadRateSchedule {
  start: string;
  end: string;
}

/**
 * Session-wide aggregate upload limit. `rate_limit_bytes_per_second` is bytes
 * per second across every torrent and peer; zero disables limiting. A null
 * schedule applies the limit around the clock.
 */
export interface UploadRateSettings {
  rate_limit_bytes_per_second: number;
  schedule: UploadRateSchedule | null;
}

/**
 * How the experimental synthetic-upload harness reports upload counters to
 * trackers. `baseline` reports the real counters (control arm), `inject`
 * fabricates a time-based counter, and `amplify` multiplies the real payload
 * transfer.
 */
export type LabMode = 'baseline' | 'inject' | 'amplify';

export interface LabSettings {
  mode: LabMode;
  /** Average synthetic growth per second in inject mode, in bytes. */
  rate_bytes_per_second: number;
  /** Reported-to-real factor in amplify mode; greater than one. */
  multiplier: number;
  /** Per-announce deviation in percent, 0-100. */
  jitter_percent: number;
  /** Per-torrent ceiling on the reported cumulative upload; zero is uncapped. */
  cap_bytes: number;
}

/**
 * Runtime-updatable part of the lab settings. Only `mode` is required by the
 * management endpoint; omitted fields fall back to the daemon defaults.
 */
export interface LabSettingsUpdate {
  mode: LabMode;
  rate_bytes_per_second?: number;
  multiplier?: number;
  jitter_percent?: number;
  cap_bytes?: number;
}

export interface LabTorrentStat {
  info_hash: string;
  real_uploaded_bytes: number;
  reported_uploaded_bytes: number;
}

export interface LabTotals {
  real_uploaded_bytes: number;
  reported_uploaded_bytes: number;
  /** reported - real; may be negative. */
  overreported_bytes: number;
}

export interface LabView {
  enabled: boolean;
  settings: LabSettings | null;
  seed: number;
  announce_log: string;
  peer_sample_interval_seconds: number;
  totals: LabTotals;
  torrents: LabTorrentStat[];
}
