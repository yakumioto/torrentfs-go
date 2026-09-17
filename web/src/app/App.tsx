import { Routes, Route } from 'react-router-dom';
import { AppLayout } from '../components/layout/AppLayout';
import { ConnectionFailure } from '../components/layout/ConnectionFailure';
import { LoadingScreen } from '../components/layout/LoadingScreen';
import { LoginPage } from '../pages/LoginPage';
import { DashboardPage } from '../pages/DashboardPage';
import { TorrentDetailPage } from '../pages/TorrentDetailPage';
import { NotFoundPage } from '../pages/NotFoundPage';
import { useAuth } from './auth-context';

export function App() {
  const auth = useAuth();

  if (auth.phase === 'probing') {
    return <LoadingScreen />;
  }
  if (auth.phase === 'login') {
    return <LoginPage />;
  }
  if (auth.phase === 'error') {
    return <ConnectionFailure message={auth.connectionError} onRetry={auth.retryProbe} />;
  }

  return (
    <Routes>
      <Route element={<AppLayout />}>
        <Route path="/" element={<DashboardPage />} />
        <Route path="/torrents/:id" element={<TorrentDetailPage />} />
        <Route path="*" element={<NotFoundPage />} />
      </Route>
    </Routes>
  );
}
