import { IconLock, IconRadar } from '@tabler/icons-react';
import { useAuth } from '../../app/auth-context';

export function ConnectionPill() {
  const auth = useAuth();
  const authenticated = auth.isAuthenticated;
  return (
    <span className={`connection-pill ${authenticated ? 'connection-pill--auth' : 'connection-pill--live'}`}>
      {authenticated ? <IconLock size={13} aria-hidden="true" /> : <IconRadar size={13} aria-hidden="true" />}
      <span>{authenticated ? 'secured session' : 'local link'}</span>
      <span className="connection-pill__dot" aria-hidden="true" />
    </span>
  );
}
