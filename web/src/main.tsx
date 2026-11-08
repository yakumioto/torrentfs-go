import '@mantine/core/styles.css';
import '@mantine/notifications/styles.css';
import './styles/tokens.css';
import './styles/global.css';

import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { createBrowserRouter, RouterProvider } from 'react-router-dom';
import { MantineProvider } from '@mantine/core';
import { Notifications } from '@mantine/notifications';
import { App } from './app/App';
import { AppProviders } from './app/providers';
import { theme } from './styles/theme';

const root = document.getElementById('root');

if (root === null) {
  throw new Error('Root element is missing.');
}

// A data router provides the navigation blocker the configuration page uses
// for unsaved changes, including the browser's back/forward buttons. App owns
// the application routes and the authentication gate.
const router = createBrowserRouter([{ path: '*', element: <App /> }]);

createRoot(root).render(
  <StrictMode>
    <MantineProvider theme={theme} defaultColorScheme="light">
      <Notifications position="top-right" />
      <AppProviders>
        <RouterProvider router={router} />
      </AppProviders>
    </MantineProvider>
  </StrictMode>,
);
