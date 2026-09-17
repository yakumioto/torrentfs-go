import { Button } from '@mantine/core';
import { IconCompassOff } from '@tabler/icons-react';
import { useNavigate } from 'react-router-dom';

export function NotFoundPage() {
  const navigate = useNavigate();
  return (
    <div className="panel error-state">
      <IconCompassOff size={34} aria-hidden="true" />
      <h2>Page not found</h2>
      <p>The requested TorrentFS route does not exist.</p>
      <Button variant="light" color="mint" onClick={() => navigate('/')}>Back to dashboard</Button>
    </div>
  );
}
