import { IconCircleCheck, IconServer } from '@tabler/icons-react';
import { useAuth } from '../../app/auth-context';

export function ConnectionPill() {
  const auth = useAuth();
  const authenticated = auth.isAuthenticated;
  return (
    <span className={`connection-pill ${authenticated ? 'connection-pill--auth' : 'connection-pill--live'}`} role="status">
      {authenticated ? <IconCircleCheck size={14} aria-hidden="true" /> : <IconServer size={14} aria-hidden="true" />}
      <span>{authenticated ? 'Connected' : 'Local daemon'}</span>
    </span>
  );
}
