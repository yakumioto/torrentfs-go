import { ActionIcon, Button, Tooltip } from '@mantine/core';
import { IconLogout, IconPlus, IconRefresh, IconRadar } from '@tabler/icons-react';
import { Link, useLocation, useNavigate } from 'react-router-dom';
import { useQueryClient } from '@tanstack/react-query';
import { useAuth } from '../../app/auth-context';
import { queryKeys } from '../../queries/keys';
import { ConnectionPill } from './ConnectionPill';

export function Header({ onAdd }: { onAdd: () => void }) {
  const auth = useAuth();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const location = useLocation();
  const isDashboard = location.pathname === '/';

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.torrents });
    if (!isDashboard) {
      void queryClient.invalidateQueries({ queryKey: queryKeys.torrentStatus(location.pathname.split('/').pop() ?? '') });
    }
  };

  return (
    <div className="app-header">
      <div className="app-header__inner">
        <Link to="/" className="brand-lockup" aria-label="TorrentFS dashboard">
          <span className="brand-mark" aria-hidden="true"><IconRadar size={20} /></span>
          <span>
            <span className="brand-name">TorrentFS</span>
            <span className="brand-caption">control room</span>
          </span>
        </Link>
        <div className="header-actions">
          <ConnectionPill />
          <Tooltip label="Refresh data">
            <ActionIcon variant="subtle" color="gray" onClick={refresh} aria-label="Refresh data">
              <IconRefresh size={18} />
            </ActionIcon>
          </Tooltip>
          <Button color="mint" leftSection={<IconPlus size={17} />} onClick={onAdd} size="sm">
            Add torrent
          </Button>
          {auth.isAuthenticated && (
            <Tooltip label="Log out">
              <ActionIcon
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
