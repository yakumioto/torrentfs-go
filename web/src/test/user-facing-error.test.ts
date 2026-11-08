import { describe, expect, it } from 'vitest';
import { ApiError } from '../api/errors';
import {
  configSaveDurabilityUnconfirmed,
  configSaveFailure,
  labSettingsApplied,
  userFacingConfigSaveError,
  userFacingError,
  userFacingLabError,
  userFacingOperationCode,
  userFacingOperationError,
  userFacingSubtitleError,
} from '../utils/user-facing-error';

describe('user-facing error mapping', () => {
  it('maps known API statuses to Chinese copy', () => {
    expect(userFacingError(new ApiError(401, 'unauthorized'))).toBe('登录凭据无效或会话已过期。');
    expect(userFacingError(new ApiError(413, 'payload too large'))).toBe('上传的种子文件超过后台服务的大小限制。');
    expect(userFacingError(new ApiError(500, 'secret server detail'))).toBe('后台服务暂时无法完成此操作，请稍后重试。');
  });

  it('does not expose unknown server strings in operation feedback', () => {
    expect(userFacingOperationError('secret server detail')).toBe('后台服务未能完成删除任务。');
    expect(userFacingOperationError(undefined, '自定义提示')).toBe('自定义提示');
  });

  it('maps subtitle failures by stable code, never by torrent status copy', () => {
    const mismatch = userFacingSubtitleError(new ApiError(415, '字幕文件名必须与目标视频的名称严格对应。', '', 'subtitle_name_mismatch'));
    expect(mismatch).toContain('Movie.2026.srt');
    // 415 alone would say "only .torrent uploads", which is wrong for subtitles.
    expect(mismatch).not.toContain('.torrent');

    const full = userFacingSubtitleError(new ApiError(507, 'subtitle storage is full', '', 'subtitle_storage_full'));
    expect(full).toContain('空间不足');

    const conflict = userFacingSubtitleError(new ApiError(409, 'subtitle path belongs to torrent payload', '', 'subtitle_payload_conflict'));
    expect(conflict).toContain('不会覆盖原始内容');

    // An unknown code must fall back rather than relay server text.
    expect(userFacingSubtitleError(new ApiError(500, 'internal path /torrents/secret', '', 'new_code'))).toBe('后台服务未能完成字幕上传，请稍后重试。');
  });

  it('explains a subtitle cleanup failure and its retry', () => {
    expect(userFacingOperationCode('subtitle_cleanup_failed')).toContain('再次删除');
    expect(userFacingOperationCode(undefined)).toBe('后台服务未能完成删除任务。');
  });

  it('maps lab failures by stable code and never relays server text', () => {
    expect(userFacingLabError(new ApiError(400, 'internal detail', '', 'invalid_lab_settings'))).toContain('实验设置内容无效');
    expect(userFacingLabError(new ApiError(409, 'disabled', '', 'lab_disabled'))).toContain('TORRENTFS_EXPERIMENTS_UPLOAD_MODE');
    expect(userFacingLabError(new ApiError(503, 'storage', '', 'lab_settings_storage_unavailable'))).toContain('.metadata 目录权限');
    expect(userFacingLabError(new ApiError(503, 'internal path /secret', '', 'new_lab_code'))).toBe('上传实验设置保存失败，请稍后重试。');
    expect(userFacingLabError('network down')).toBe('上传实验设置保存失败，请稍后重试。');

    const applied = new ApiError(503, 'durability', '', 'lab_settings_durability_unconfirmed');
    expect(userFacingLabError(applied)).toContain('已生效');
    expect(labSettingsApplied(applied)).toBe(true);
    expect(labSettingsApplied(new ApiError(503, 'storage', '', 'lab_settings_storage_unavailable'))).toBe(false);
    expect(labSettingsApplied(undefined)).toBe(false);
  });
});

describe('configuration save error mapping', () => {
  it('keeps the per-key errors and the applied keys of a structured failure', () => {
    const failure = new ApiError(503, '内部错误路径 /etc/secret', '', 'durability_unconfirmed', {
      error: '新配置已生效，但无法确认重启后仍能保留',
      code: 'durability_unconfirmed',
      field_errors: [{ path: 'cache.capacity', message: 'must be positive' }],
      applied: ['cache.capacity'],
      restart_required: [],
      persisted: false,
    });

    expect(configSaveFailure(failure)?.field_errors).toEqual([{ path: 'cache.capacity', message: 'must be positive' }]);
    expect(configSaveFailure(failure)?.applied).toEqual(['cache.capacity']);
    expect(userFacingConfigSaveError(failure)).toContain('已生效');
    expect(configSaveDurabilityUnconfirmed(failure)).toBe(true);
    expect(configSaveDurabilityUnconfirmed(new ApiError(409, 'conflict', '', 'conflict'))).toBe(false);
  });

  it('maps every stable code to Chinese text and never relays server text', () => {
    for (const code of ['invalid', 'conflict', 'read_only', 'not_configured', 'persistence_unavailable']) {
      const error = new ApiError(409, 'internal path /secret', '', code);
      expect(userFacingConfigSaveError(error)).not.toContain('/secret');
      expect(userFacingConfigSaveError(error)).not.toBe('配置保存失败，请稍后重试。');
    }
    expect(userFacingConfigSaveError('network down')).toBe('配置保存失败，请稍后重试。');
    expect(configSaveFailure(new ApiError(409, 'conflict', '', 'conflict'))).toBeUndefined();
  });
});
