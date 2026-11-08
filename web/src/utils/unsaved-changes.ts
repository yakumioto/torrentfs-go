import { useEffect } from 'react';
import { useBlocker } from 'react-router-dom';

export interface UnsavedChangesGuard {
  /** The in-app destination the user tried to open while the page was dirty. */
  pendingHref: string | null;
  /** Navigates to the pending destination, abandoning the unsaved changes. */
  leave: () => void;
  /** Keeps the user on the page together with the unsaved changes. */
  stay: () => void;
}

/**
 * Guards page unload and every in-app navigation while a form has unsaved
 * changes.
 *
 * The application uses a data router, so the router's own blocker API covers
 * link clicks, programmatic navigation, and the browser's back/forward
 * buttons. A hash change on the same page and a click on the page already
 * open are not navigations away, so they do not interrupt the user.
 */
export function useUnsavedChangesGuard(dirty: boolean): UnsavedChangesGuard {
  const blocker = useBlocker(({ currentLocation, nextLocation }) =>
    dirty
    && (nextLocation.pathname !== currentLocation.pathname
      || nextLocation.search !== currentLocation.search));

  useEffect(() => {
    if (!dirty) {
      return;
    }
    const handleBeforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      // Browsers show their own generic confirmation text.
      event.returnValue = '';
    };
    window.addEventListener('beforeunload', handleBeforeUnload);
    return () => window.removeEventListener('beforeunload', handleBeforeUnload);
  }, [dirty]);

  const blocked = blocker.state === 'blocked';
  return {
    pendingHref: blocked
      ? `${blocker.location.pathname}${blocker.location.search}${blocker.location.hash}`
      : null,
    leave: () => {
      if (blocker.state === 'blocked') {
        blocker.proceed();
      }
    },
    stay: () => {
      if (blocker.state === 'blocked') {
        blocker.reset();
      }
    },
  };
}
