import { MantineProvider } from '@mantine/core';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import { AppLayout } from '../components/layout/AppLayout';
import { theme } from '../styles/theme';

afterEach(cleanup);

vi.mock('../app/auth-context', () => ({
  useAuth: () => ({ api: {} }),
}));

vi.mock('../components/layout/Header', () => ({
  Header: ({ onAdd }: { onAdd: () => void }) => <button onClick={onAdd}>打开添加弹窗</button>,
}));

vi.mock('../components/dialogs/AddTorrentDialog', () => ({
  AddTorrentDialog: ({ opened, onAdded }: { opened: boolean; onAdded: (ids: string[]) => void }) => opened ? (
    <div>
      <button onClick={() => onAdded(['torrent-1'])}>完成单任务</button>
      <button onClick={() => onAdded(['torrent-1', 'torrent-2'])}>完成多任务</button>
    </div>
  ) : null,
}));

function LocationDisplay() {
  return <span data-testid="location">{useLocation().pathname}</span>;
}

function renderLayout() {
  return render(
    <MantineProvider theme={theme} defaultColorScheme="light">
      <MemoryRouter initialEntries={['/']}>
        <Routes>
          <Route element={<AppLayout />}>
            <Route path="*" element={<LocationDisplay />} />
          </Route>
        </Routes>
      </MemoryRouter>
    </MantineProvider>,
  );
}

describe('AppLayout add-task navigation', () => {
  it('opens the single task detail route for one unique task', () => {
    renderLayout();

    fireEvent.click(screen.getByRole('button', { name: '打开添加弹窗' }));
    fireEvent.click(screen.getByRole('button', { name: '完成单任务' }));

    expect(screen.getByTestId('location')).toHaveTextContent('/torrents/torrent-1');
  });

  it('returns to the task list for multiple unique tasks', () => {
    renderLayout();

    fireEvent.click(screen.getByRole('button', { name: '打开添加弹窗' }));
    fireEvent.click(screen.getByRole('button', { name: '完成多任务' }));

    expect(screen.getByTestId('location')).toHaveTextContent('/');
  });
});
