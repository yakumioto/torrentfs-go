import { Button } from '@mantine/core';
import { IconCompassOff } from '@tabler/icons-react';
import { useNavigate } from 'react-router-dom';

export function NotFoundPage() {
  const navigate = useNavigate();
  return (
    <div className="panel error-state" role="alert">
      <IconCompassOff size={34} aria-hidden="true" />
      <h2>页面不存在</h2>
      <p>你访问的 TorrentFS 页面不存在。</p>
      <Button variant="light" color="torrent" onClick={() => navigate('/')}>返回任务列表</Button>
    </div>
  );
}
