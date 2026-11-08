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
  /** Live real upload counter for this torrent. */
  real_uploaded_bytes: number;
  /** Real upload counter captured when the last announce was sent. */
  real_uploaded_at_last_announce_bytes: number;
  reported_uploaded_bytes: number;
  /** RFC3339 timestamp of the last announce; may be null when unknown. */
  last_announce_at?: string | null;
}

export interface LabTotals {
  /** Live real upload counter since start. */
  real_uploaded_bytes: number;
  /** Real upload counter at the moment the last announce was sent. */
  real_uploaded_at_last_announce_bytes: number;
  reported_uploaded_bytes: number;
  /** reported - real_at_last_announce; may be negative. */
  overreported_bytes: number;
  /** real_live - real_at_last_announce: real bytes not yet part of a report. */
  pending_real_upload_bytes: number;
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

/** Where a configuration value came from, in precedence order. */
export type ConfigSource = 'default' | 'file' | 'environment';

/** A stable machine-readable reason a configuration key cannot be edited. */
export type ConfigKeyReadOnlyReason = 'environment_override' | 'secret';

/** A stable machine-readable reason the whole configuration page is read-only. */
export type ConfigPageReadOnlyReason = 'not_configured' | 'file_not_writable';

/**
 * One configuration key as the read-only configuration page sees it. The
 * schema metadata drives the grouping, the labels and the apply badge; the
 * values come from the file layer and from the effective configuration.
 */
export interface ConfigKey {
  path: string;
  /** The configuration section that groups the key; empty for top-level keys. */
  section: string;
  /** The schema type: string, bool, int, int64, duration, byte_quantity, byte_rate, percent, ratio, list, or enum. */
  kind: string;
  /** The accepted values of an enumerated key; empty for every other kind. */
  values: string[];
  default: string;
  /** The file layer value, before the environment is merged. A secret is empty. */
  file_value: string;
  /** The value the process uses. A secret is empty. */
  effective_value: string;
  source: ConfigSource | string;
  environment: string;
  overridable: boolean;
  secret: boolean;
  component: string;
  hot_apply: boolean;
  documentation: string;
  read_only_reason: ConfigKeyReadOnlyReason | '';
}

/** The outcome of the most recent save or reload. */
export interface ConfigOperationStatus {
  at: string | null;
  ok: boolean;
  error: string;
  keys: string[];
}

/** The read-only configuration view: schema metadata, values, sources and read-only reasons. */
export interface ConfigView {
  /** The configuration file the process loaded; empty when it started without one. */
  config_file: string;
  /**
   * Digest of the configuration file the view was rendered from. A save sends
   * it back; a file another writer changed has a different digest, so the save
   * is refused instead of overwriting the change. Empty without a file.
   */
  baseline: string;
  read_only_reason: ConfigPageReadOnlyReason | '';
  /**
   * Why the configuration file on disk cannot be used right now, for example
   * an external edit that made it invalid. The values of the view stay those
   * of the last successful load. Empty means the file still matches them.
   */
  file_error: string;
  keys: ConfigKey[];
  last_save: ConfigOperationStatus;
  last_reload: ConfigOperationStatus;
}

/** A sparse save: changed configuration keys by path, and the file baseline. */
export interface ConfigSaveRequest {
  baseline: string;
  patch: Record<string, string>;
}

/** The changed keys that took effect and those that wait for a restart. */
export interface ConfigSaveResult {
  applied: string[];
  restart_required: string[];
  /** Whether the new file is confirmed to survive a crash. */
  persisted: boolean;
}

/** One rejected configuration key of a save. */
export interface ConfigFieldError {
  path: string;
  message: string;
}

/** Stable machine-readable reasons a configuration save was rejected. */
export type ConfigSaveErrorCode =
  | 'invalid'
  | 'conflict'
  | 'read_only'
  | 'not_configured'
  | 'persistence_unavailable'
  | 'durability_unconfirmed';

/**
 * The failure body of a rejected save. `applied` is non-empty for a durability
 * failure: the new values are already in force, only the restart-recovery
 * guarantee is unproven.
 */
export interface ConfigSaveFailure {
  error: string;
  code: ConfigSaveErrorCode | string;
  field_errors: ConfigFieldError[];
  applied: string[];
  restart_required: string[];
  persisted: boolean;
}
