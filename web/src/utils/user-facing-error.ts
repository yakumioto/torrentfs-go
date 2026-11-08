import { ApiError } from '../api/errors';
import type { ConfigFieldError, ConfigSaveFailure } from '../types/api';

const STATUS_MESSAGES: Record<number, string> = {
  400: '请求内容无效，请检查输入后重试。',
  401: '登录凭据无效或会话已过期。',
  403: '当前账号没有执行此操作的权限。',
  404: '请求的任务或操作不存在，可能已被后台服务移除。',
  409: '当前任务仍被其他资源引用，请先解除引用后重试。',
  413: '上传的种子文件超过后台服务的大小限制。',
  415: '后台服务仅接受 .torrent 文件上传或磁力链接请求。',
};

/**
 * Subtitle upload errors are mapped by stable code, never by server text. The
 * .torrent copy above would be misleading here, so subtitles never reuse it.
 */
const SUBTITLE_CODE_MESSAGES: Record<string, string> = {
  subtitle_name_mismatch: '字幕文件名必须与所选视频的名称完全一致，例如视频为 Movie.2026.mkv 时应上传 Movie.2026.srt。',
  subtitle_format_unsupported: '仅支持小写扩展名的 .srt、.ass 或 .vtt 字幕文件。',
  subtitle_video_not_found: '所选视频已不可用，请刷新任务状态后重新选择。',
  subtitle_name_conflict: '该视频的字幕名称在挂载点中存在冲突，无法安全上传。',
  subtitle_payload_conflict: '目标路径属于种子自带文件，TorrentFS 不会覆盖原始内容。',
  subtitle_storage_unavailable: '字幕存储当前不可用，请检查后台服务的目录权限后重试。',
  subtitle_storage_full: '字幕存储空间不足，请释放磁盘空间后重试。',
  subtitle_write_failed: '字幕文件写入失败，请稍后重试。',
  torrent_deleting: '任务正在删除，删除完成后无法再上传字幕。',
  subtitle_upload_too_large: '字幕文件超过后台服务的大小限制。',
};

export function userFacingError(error: unknown, fallback = '后台服务暂时无法完成此操作，请稍后重试。'): string {
  if (error instanceof ApiError) {
    return STATUS_MESSAGES[error.status] ?? fallback;
  }
  return fallback;
}

/**
 * Upload-rate save failures are mapped by stable code. "Applied but durability
 * unconfirmed" must not read like a plain failure: the new limit is already in
 * force, only the restart-recovery guarantee is unproven.
 */
const UPLOAD_RATE_CODE_MESSAGES: Record<string, string> = {
  invalid_upload_rate_settings: '设置内容无效，请检查上传上限和时段后重试。',
  upload_rate_settings_storage_unavailable: '设置未能写入，当前限速未改变；请检查 .metadata 目录权限和磁盘空间后重试。',
  upload_rate_settings_durability_unconfirmed: '新规则已生效，但无法确认重启后仍能恢复；请重新打开设置确认当前状态。',
};

export function userFacingUploadRateError(error: unknown, fallback = '上传限速设置保存失败，请稍后重试。'): string {
  if (error instanceof ApiError) {
    return UPLOAD_RATE_CODE_MESSAGES[error.code] ?? fallback;
  }
  return fallback;
}

/** True when the daemon already applied the new settings despite the error. */
export function uploadRateSettingsApplied(error: unknown): boolean {
  return error instanceof ApiError && error.code === 'upload_rate_settings_durability_unconfirmed';
}

/**
 * Lab save failures are mapped by stable code. As with the upload rate, the
 * "durability unconfirmed" case is not a plain failure: the new experiment
 * configuration is already in force, only the restart-recovery guarantee is
 * unproven.
 */
const LAB_CODE_MESSAGES: Record<string, string> = {
  invalid_lab_settings: '实验设置内容无效，请检查模式、速率、倍率和上限后重试。',
  lab_disabled: '上传实验未启用，需要设置 TORRENTFS_EXPERIMENTS_UPLOAD_MODE 等环境变量并重启后台服务后才能保存。',
  lab_settings_storage_unavailable: '实验设置未能写入，当前配置未改变；请检查 .metadata 目录权限和磁盘空间后重试。',
  lab_settings_durability_unconfirmed: '新实验设置已生效，但无法确认重启后仍能恢复；请重新读取确认当前状态。',
};

export function userFacingLabError(error: unknown, fallback = '上传实验设置保存失败，请稍后重试。'): string {
  if (error instanceof ApiError) {
    return LAB_CODE_MESSAGES[error.code] ?? fallback;
  }
  return fallback;
}

/** True when the daemon already applied the new lab settings despite the error. */
export function labSettingsApplied(error: unknown): boolean {
  return error instanceof ApiError && error.code === 'lab_settings_durability_unconfirmed';
}

export function userFacingSubtitleError(error: unknown, fallback = '后台服务未能完成字幕上传，请稍后重试。'): string {
  if (error instanceof ApiError) {
    return SUBTITLE_CODE_MESSAGES[error.code] ?? fallback;
  }
  return fallback;
}

export function userFacingOperationError(error: unknown, fallback = '后台服务未能完成删除任务。'): string {
  return userFacingError(error, fallback);
}

export function userFacingOperationCode(code: string | undefined, fallback = '后台服务未能完成删除任务。'): string {
  if (code === 'subtitle_cleanup_failed') {
    return '字幕文件清理失败。修复存储权限或空间后，可以再次删除同一任务完成重试。';
  }
  return fallback;
}

/**
 * Configuration save failures are mapped by stable code. The durability case is
 * not a plain failure: the new values are already in force, only the
 * restart-recovery guarantee is unproven.
 */
const CONFIG_SAVE_CODE_MESSAGES: Record<string, string> = {
  invalid: '配置校验失败，请修正标记的配置项后重试。',
  conflict: '配置文件已被外部修改，请重新读取页面后再保存。',
  read_only: '配置为只读，无法保存；请检查环境变量覆盖与配置文件权限。',
  not_configured: '后台服务没有加载配置文件，无法保存。',
  persistence_unavailable: '配置未能写入，文件与当前运行值均未改变；请检查磁盘空间和目录权限。',
  durability_unconfirmed: '新配置已生效，但无法确认重启后仍能保留。',
};

function stringList(value: unknown): string[] {
  if (!Array.isArray(value)) {
    return [];
  }
  return value.filter((item): item is string => typeof item === 'string');
}

function fieldErrorList(value: unknown): ConfigFieldError[] {
  if (!Array.isArray(value)) {
    return [];
  }
  const fields: ConfigFieldError[] = [];
  for (const item of value) {
    if (typeof item !== 'object' || item === null) {
      continue;
    }
    const record = item as Record<string, unknown>;
    if (typeof record.path === 'string' && typeof record.message === 'string') {
      fields.push({ path: record.path, message: record.message });
    }
  }
  return fields;
}

/**
 * Returns the structured failure body of a rejected configuration save, or
 * undefined when the error is not one. The page uses the per-key errors and the
 * applied keys, so a display message alone is not enough.
 */
export function configSaveFailure(error: unknown): ConfigSaveFailure | undefined {
  if (!(error instanceof ApiError) || typeof error.payload !== 'object' || error.payload === null) {
    return undefined;
  }
  const record = error.payload as Record<string, unknown>;
  if (typeof record.code !== 'string') {
    return undefined;
  }
  return {
    error: typeof record.error === 'string' ? record.error : error.serverMessage,
    code: record.code,
    field_errors: fieldErrorList(record.field_errors),
    applied: stringList(record.applied),
    restart_required: stringList(record.restart_required),
    persisted: record.persisted === true,
  };
}

export function userFacingConfigSaveError(error: unknown, fallback = '配置保存失败，请稍后重试。'): string {
  const failure = configSaveFailure(error);
  if (failure === undefined) {
    return fallback;
  }
  return CONFIG_SAVE_CODE_MESSAGES[failure.code] ?? fallback;
}

/** True when the daemon already applied the new configuration despite the error. */
export function configSaveDurabilityUnconfirmed(error: unknown): boolean {
  return configSaveFailure(error)?.code === 'durability_unconfirmed';
}
