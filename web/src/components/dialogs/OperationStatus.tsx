import { IconAlertTriangle, IconCircleCheck, IconLoader } from '@tabler/icons-react';
import type { Operation } from '../../types/api';
import { ApiError, errorMessage } from '../../api/errors';
import styles from './OperationStatus.module.css';

export function OperationStatus({ operation, error }: { operation?: Operation; error?: unknown }) {
  if (error !== undefined && error !== null) {
    const missing = error instanceof ApiError && error.status === 404;
    return <div className={styles.status} role="alert"><IconAlertTriangle size={19} color="var(--coral)" aria-hidden="true" /><div className={styles.copy}><div className={styles.title}>{missing ? 'Operation is no longer queryable' : 'Operation status unavailable'}</div><div className={styles.detail}>{missing ? 'The daemon may have restarted. Refresh the task list to see its current state.' : errorMessage(error)}</div></div></div>;
  }
  if (operation === undefined) {
    return null;
  }
  if (operation.state === 'deleted') {
    return <div className={styles.status} role="status"><IconCircleCheck size={19} color="var(--sea)" aria-hidden="true" /><div className={styles.copy}><div className={styles.title}>Torrent deleted</div><div className={styles.detail}>The task and its managed metadata are gone from the board.</div></div></div>;
  }
  if (operation.state === 'delete_failed') {
    return <div className={styles.status} role="alert"><IconAlertTriangle size={19} color="var(--coral)" aria-hidden="true" /><div className={styles.copy}><div className={styles.title}>Delete failed</div><div className={styles.detail}>{operation.error || 'The daemon could not finish deleting this torrent.'}</div></div></div>;
  }
  return <div className={styles.status} role="status"><IconLoader size={19} color="var(--amber)" aria-hidden="true" /><div className={styles.copy}><div className={styles.title}>Deleting torrent…</div><div className={styles.detail}>The task stays visible until the asynchronous operation reaches a terminal state.</div></div></div>;
}
