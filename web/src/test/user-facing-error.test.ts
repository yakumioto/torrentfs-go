import { describe, expect, it } from 'vitest';
import { ApiError } from '../api/errors';
import { userFacingError, userFacingOperationCode, userFacingOperationError, userFacingSubtitleError } from '../utils/user-facing-error';

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
});
