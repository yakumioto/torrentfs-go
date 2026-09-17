import { IconCircleCheck, IconServer } from '@tabler/icons-react';
import { useAuth } from '../../app/auth-context';
import styles from './ConnectionPill.module.css';

export function ConnectionPill() {
  const auth = useAuth();
  const authenticated = auth.isAuthenticated;
  return (
    <span className={`${styles.pill} ${authenticated ? styles.pillAuth : styles.pillLive}`} role="status">
      {authenticated ? <IconCircleCheck size={14} aria-hidden="true" /> : <IconServer size={14} aria-hidden="true" />}
      <span className={styles.label}>{authenticated ? 'Connected' : 'Local daemon'}</span>
    </span>
  );
}
