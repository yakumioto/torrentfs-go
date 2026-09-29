import { ApiError } from '../api/errors';

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
