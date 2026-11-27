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
    return <div className={styles.status} role="status"><IconCircleCheck size={19} color="var(--success)" aria-hidden="true" /><div className={styles.copy}><div className={styles.title}>任务已删除</div><div className={styles.detail}>任务、其元数据及由 TorrentFS 管理的字幕文件已从列表中移除。</div>{operation.warning !== undefined && operation.warning !== '' && <div className={styles.detail}>停止上报未送达 tracker，上传量可能延后结算：{operation.warning}</div>}</div></div>;
  }
  if (operation.state === 'delete_failed') {
    const cleanupFailed = operation.error_code === 'subtitle_cleanup_failed';
    return <div className={styles.status} role="alert"><IconAlertTriangle size={19} color="var(--danger)" aria-hidden="true" /><div className={styles.copy}><div className={styles.title}>{cleanupFailed ? '字幕文件清理失败' : '删除失败'}</div><div className={styles.detail}>{cleanupFailed ? userFacingOperationCode(operation.error_code) : userFacingOperationError(operation.error, '后台服务未能完成删除任务，请稍后重试。')}</div><div className={styles.detail}>任务仍会保持隐藏状态，不会重新出现在列表中；再次删除同一任务即可重试。</div></div></div>;
  }
  if (operation.state === 'deleting') {
    return <div className={styles.status} role="status"><IconLoader size={19} color="var(--warning)" aria-hidden="true" /><div className={styles.copy}><div className={styles.title}>正在删除任务…</div><div className={styles.detail}>异步操作完成前，任务仍会保留在当前列表中；字幕文件会在删除完成时一并清理。</div></div></div>;
  }
  if (operation.state === 'completed') {
    return <div className={styles.status} role="status"><IconCircleCheck size={19} color="var(--success)" aria-hidden="true" /><div className={styles.copy}><div className={styles.title}>任务已移交</div><div className={styles.detail}>种子已交给 Transmission，并已从 TorrentFS 中删除；分片缓存与元数据随之释放。</div>{operation.warning !== undefined && operation.warning !== '' && <div className={styles.detail}>停止上报未送达 tracker，上传量可能延后结算：{operation.warning}</div>}</div></div>;
  }
  if (operation.state === 'restored') {
    return <div className={styles.status} role="alert"><IconAlertTriangle size={19} color="var(--danger)" aria-hidden="true" /><div className={styles.copy}><div className={styles.title}>移交失败，任务已复原</div><div className={styles.detail}>{userFacingOperationCode(operation.error_code, '后台服务未能把种子交给 Transmission。')}</div><div className={styles.detail}>任务已恢复原来的做种状态，可以修正配置后重试。</div></div></div>;
  }
  if (operation.state === 'failed') {
    return <div className={styles.status} role="alert"><IconAlertTriangle size={19} color="var(--danger)" aria-hidden="true" /><div className={styles.copy}><div className={styles.title}>已移交，但本机删除失败</div><div className={styles.detail}>种子已由 Transmission 接收，但从 TorrentFS 删除未完成；再次删除同一任务即可重试清理。</div><div className={styles.detail}>{userFacingOperationError(operation.error, '后台服务未能完成本机删除。')}</div></div></div>;
  }
  return <div className={styles.status} role="status"><IconLoader size={19} color="var(--warning)" aria-hidden="true" /><div className={styles.copy}><div className={styles.title}>正在移交任务…</div><div className={styles.detail}>正在暂停做种并把 .torrent 交给 Transmission；成功后从本机删除，失败会自动复原。</div></div></div>;
}
