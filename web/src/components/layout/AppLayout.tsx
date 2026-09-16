import { AppShell } from '@mantine/core';
import { Outlet, useNavigate } from 'react-router-dom';
import { useState } from 'react';
import { Header } from './Header';
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
    <AppShell header={{ height: 76 }} className="app-frame">
      <AppShell.Header className="app-header">
        <Header onAdd={() => setAddOpen(true)} />
      </AppShell.Header>
      <AppShell.Main className="page-main">
        <div className="page-container">
          <Outlet context={{ openAddTorrent: () => setAddOpen(true) } satisfies AppOutletContext} />
        </div>
      </AppShell.Main>
      <AddTorrentDialog
        opened={addOpen}
        onClose={() => setAddOpen(false)}
        api={auth.api}
        onAdded={(id) => {
          setAddOpen(false);
          navigate(`/torrents/${id}`);
        }}
      />
    </AppShell>
  );
}
