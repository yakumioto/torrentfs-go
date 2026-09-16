import { Center, Loader } from '@mantine/core';

export function LoadingScreen() {
  return (
    <Center className="auth-page" role="status" aria-label="Connecting to TorrentFS">
      <Loader color="mint" size="md" />
    </Center>
  );
}
