import { Loader } from '@mantine/core';
import shell from '../../styles/auth-shell.module.css';

export function LoadingScreen() {
  return (
    <main className={`${shell.page} ${shell.authPage}`} role="status" aria-label="正在连接 TorrentFS 服务">
      <Loader color="torrent" size="md" />
    </main>
  );
}
