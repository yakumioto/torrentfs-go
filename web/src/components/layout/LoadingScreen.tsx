import { Center, Loader } from '@mantine/core';
import shell from '../../styles/auth-shell.module.css';

export function LoadingScreen() {
  return (
    <Center className={shell.page} role="status" aria-label="Connecting to TorrentFS">
      <Loader color="mint" size="md" />
    </Center>
  );
}
