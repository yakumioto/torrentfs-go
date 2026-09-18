import { IconCircleCheck, IconServer } from '@tabler/icons-react';
import { useAuth } from '../../app/auth-context';
import styles from './ConnectionPill.module.css';

export function ConnectionPill() {
  const auth = useAuth();
  const authenticated = auth.isAuthenticated;
  const label = authenticated ? '已认证连接' : '本地匿名服务';
  return (
    <span className={`${styles.pill} ${authenticated ? styles.pillAuth : styles.pillLive}`} role="status" aria-label={label} title={label}>
      {authenticated ? <IconCircleCheck size={14} aria-hidden="true" /> : <IconServer size={14} aria-hidden="true" />}
      <span className={styles.label}>{label}</span>
    </span>
  );
}
