import { IconAlertTriangle, IconCircleCheck, IconLoader } from '@tabler/icons-react';
import type { Operation } from '../../types/api';
import { ApiError } from '../../api/errors';
import { userFacingOperationCode, userFacingOperationError } from '../../utils/user-facing-error';
import styles from './OperationStatus.module.css';

export function OperationStatus({ operation, error }: { operation?: Operation; error?: unknown }) {
  if (error !== undefined && error !== null) {
    const missing = error instanceof ApiError && error.status === 404;
    return <div className={styles.status} role="alert"><IconAlertTriangle size={19} color="var(--danger)" aria-hidden="true" /><div className={styles.copy}><div className={styles.title}>{missing ? '操作已无法查询' : '操作状态暂不可用'}</div><div className={styles.detail}>{missing ? '后台服务可能已经重启，请刷新任务列表确认当前状态。' : userFacingOperationError(error, '暂时无法获取删除操作状态。')}</div></div></div>;
  }
  if (operation === undefined) {
    return null;
  }
  if (operation.state === 'deleted') {
    return <div className={styles.status} role="status"><IconCircleCheck size={19} color="var(--success)" aria-hidden="true" /><div className={styles.copy}><div className={styles.title}>任务已删除</div><div className={styles.detail}>任务、其元数据及由 TorrentFS 管理的字幕文件已从列表中移除。</div></div></div>;
  }
  if (operation.state === 'delete_failed') {
    const cleanupFailed = operation.error_code === 'subtitle_cleanup_failed';
    return <div className={styles.status} role="alert"><IconAlertTriangle size={19} color="var(--danger)" aria-hidden="true" /><div className={styles.copy}><div className={styles.title}>{cleanupFailed ? '字幕文件清理失败' : '删除失败'}</div><div className={styles.detail}>{cleanupFailed ? userFacingOperationCode(operation.error_code) : userFacingOperationError(operation.error, '后台服务未能完成删除任务，请稍后重试。')}</div><div className={styles.detail}>任务仍会保持隐藏状态，不会重新出现在列表中；再次删除同一任务即可重试。</div></div></div>;
  }
  return <div className={styles.status} role="status"><IconLoader size={19} color="var(--warning)" aria-hidden="true" /><div className={styles.copy}><div className={styles.title}>正在删除任务…</div><div className={styles.detail}>异步操作完成前，任务仍会保留在当前列表中；字幕文件会在删除完成时一并清理。</div></div></div>;
}
