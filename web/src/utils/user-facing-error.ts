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

export function userFacingError(error: unknown, fallback = '后台服务暂时无法完成此操作，请稍后重试。'): string {
  if (error instanceof ApiError) {
    return STATUS_MESSAGES[error.status] ?? fallback;
  }
  return fallback;
}

export function userFacingOperationError(error: unknown, fallback = '后台服务未能完成删除任务。'): string {
  return userFacingError(error, fallback);
}
