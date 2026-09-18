import { describe, expect, it } from 'vitest';
import { ApiError } from '../api/errors';
import { userFacingError, userFacingOperationError } from '../utils/user-facing-error';

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
});
