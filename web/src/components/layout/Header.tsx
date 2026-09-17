import { ActionIcon, Button, Tooltip } from '@mantine/core';
import { IconBox, IconLogout, IconPlus, IconRefresh } from '@tabler/icons-react';
import type { MouseEvent } from 'react';
import { Link, useLocation, useNavigate } from 'react-router-dom';
import { useQueryClient } from '@tanstack/react-query';
import { useAuth } from '../../app/auth-context';
import { queryKeys } from '../../queries/keys';
import { ConnectionPill } from './ConnectionPill';
import styles from './Header.module.css';

export function Header({ onAdd }: { onAdd: () => void }) {
  const auth = useAuth();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const location = useLocation();
  const isDashboard = location.pathname === '/';

  const refresh = (event: MouseEvent<HTMLButtonElement>) => {
    event.preventDefault();
    event.stopPropagation();
    void queryClient.invalidateQueries({ queryKey: queryKeys.torrents });
    if (!isDashboard) {
      void queryClient.invalidateQueries({ queryKey: queryKeys.torrentStatus(location.pathname.split('/').pop() ?? '') });
    }
  };

  return (
    <div className={styles.bar}>
      <div className={styles.inner}>
        <Link to="/" className={styles.brand} aria-label="TorrentFS dashboard">
          <span className={styles.brandMark} aria-hidden="true"><IconBox size={18} /></span>
          <span className={styles.brandName}>torrentfs</span>
        </Link>
        <div className={styles.actions}>
          <ConnectionPill />
          <Tooltip label="Refresh data">
            <ActionIcon type="button" variant="subtle" color="gray" onClick={refresh} aria-label="Refresh data">
              <IconRefresh size={18} />
            </ActionIcon>
          </Tooltip>
          <Button color="mint" leftSection={<IconPlus size={17} />} onClick={onAdd} size="sm">
            Add torrent
          </Button>
          {auth.isAuthenticated && (
            <Tooltip label="Log out">
              <ActionIcon
                type="button"
                variant="subtle"
                color="gray"
                aria-label="Log out"
                onClick={() => void auth.logout().then(() => navigate('/'))}
              >
                <IconLogout size={18} />
              </ActionIcon>
            </Tooltip>
          )}
        </div>
      </div>
    </div>
  );
}
