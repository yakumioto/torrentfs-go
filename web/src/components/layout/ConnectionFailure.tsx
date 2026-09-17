import { Button, Center } from '@mantine/core';
import { IconPlugOff } from '@tabler/icons-react';
import shell from '../../styles/auth-shell.module.css';

export function ConnectionFailure({ message, onRetry }: { message: string; onRetry: () => void }) {
  return (
    <Center className={shell.page}>
      <div className={`${shell.card} error-state`} role="alert">
        <IconPlugOff size={34} aria-hidden="true" />
        <h2>The daemon is out of reach</h2>
        <p>{message || 'The HTTP service did not answer. Check the listener and try again.'}</p>
        <Button color="mint" onClick={onRetry}>Retry connection</Button>
      </div>
    </Center>
  );
}
