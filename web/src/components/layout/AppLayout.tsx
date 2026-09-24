import { AppShell } from '@mantine/core';
import { Outlet, useNavigate } from 'react-router-dom';
import { useState } from 'react';
import { Header } from './Header';
import styles from './AppLayout.module.css';
import { AddTorrentDialog } from '../dialogs/AddTorrentDialog';
import { useAuth } from '../../app/auth-context';

export interface AppOutletContext {
  openAddTorrent: () => void;
}

export function AppLayout() {
  const auth = useAuth();
  const navigate = useNavigate();
  const [addOpen, setAddOpen] = useState(false);

  return (
    <AppShell header={{ height: 56 }} className={styles.frame}>
      <AppShell.Header className={styles.header}>
        <Header onAdd={() => setAddOpen(true)} />
      </AppShell.Header>
      <AppShell.Main className={styles.main}>
        <div className={styles.container}>
          <Outlet context={{ openAddTorrent: () => setAddOpen(true) } satisfies AppOutletContext} />
        </div>
      </AppShell.Main>
      <AddTorrentDialog
        opened={addOpen}
        onClose={() => setAddOpen(false)}
        api={auth.api}
        onAdded={(ids) => {
          setAddOpen(false);
          if (ids.length === 1) {
            navigate(`/torrents/${ids[0]}`);
          } else if (ids.length > 1) {
            navigate('/');
          }
        }}
      />
    </AppShell>
  );
}
