import { Button } from '@mantine/core';
import { IconPlugOff } from '@tabler/icons-react';
import shell from '../../styles/auth-shell.module.css';

export function ConnectionFailure({ message, onRetry }: { message: string; onRetry: () => void }) {
  return (
    <main className={`${shell.page} ${shell.authPage}`}>
      <div className={`${shell.card} error-state`} role="alert">
        <IconPlugOff size={34} aria-hidden="true" />
        <h2>无法连接 TorrentFS 服务</h2>
        <p>{message || '后台服务没有响应，请检查服务是否正在运行。'}</p>
        <Button color="torrent" onClick={onRetry}>重试连接</Button>
      </div>
    </main>
  );
}
