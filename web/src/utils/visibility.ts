import { useEffect, useState } from 'react';

export function usePageVisible(): boolean {
  const [visible, setVisible] = useState(() => document.visibilityState === 'visible');

  useEffect(() => {
    const handleVisibilityChange = () => setVisible(document.visibilityState === 'visible');
    document.addEventListener('visibilitychange', handleVisibilityChange);
    return () => document.removeEventListener('visibilitychange', handleVisibilityChange);
  }, []);

  return visible;
}
