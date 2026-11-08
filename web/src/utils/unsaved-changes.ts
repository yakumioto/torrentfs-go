import { useEffect, useRef, useState } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';

export interface UnsavedChangesGuard {
  /** The in-app destination the user tried to open while the page was dirty. */
  pendingHref: string | null;
  /** Navigates to the pending destination, abandoning the unsaved changes. */
  leave: () => void;
  /** Keeps the user on the page together with the unsaved changes. */
  stay: () => void;
}

/**
 * Guards page unload and in-app link navigation while a form has unsaved
 * changes. The application uses the declarative router, whose blocker API is
 * only available with data routers, so link clicks are intercepted in the
 * capture phase before the router sees them.
 */
export function useUnsavedChangesGuard(dirty: boolean): UnsavedChangesGuard {
  const navigate = useNavigate();
  const location = useLocation();
  const dirtyRef = useRef(dirty);
  const locationRef = useRef(location);
  const [pendingHref, setPendingHref] = useState<string | null>(null);

  useEffect(() => {
    locationRef.current = location;
  }, [location]);

  useEffect(() => {
    dirtyRef.current = dirty;
    if (!dirty) {
      setPendingHref(null);
    }
  }, [dirty]);

  useEffect(() => {
    const handleClick = (event: MouseEvent) => {
      if (!dirtyRef.current || event.defaultPrevented) {
        return;
      }
      if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
        return;
      }
      const target = event.target;
      if (!(target instanceof Element)) {
        return;
      }
      const anchor = target.closest('a[href]');
      if (!(anchor instanceof HTMLAnchorElement)) {
        return;
      }
      if (anchor.target !== '' && anchor.target !== '_self') {
        return;
      }
      if (anchor.hasAttribute('download')) {
        return;
      }
      const href = anchor.getAttribute('href') ?? '';
      if (!href.startsWith('/') || href.startsWith('//')) {
        return;
      }
      // A link to the page already open is not a navigation worth guarding.
      if (href === locationRef.current.pathname) {
        return;
      }
      event.preventDefault();
      event.stopPropagation();
      setPendingHref(href);
    };
    document.addEventListener('click', handleClick, true);
    return () => document.removeEventListener('click', handleClick, true);
  }, []);

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

  return {
    pendingHref,
    leave: () => {
      if (pendingHref !== null) {
        setPendingHref(null);
        navigate(pendingHref);
      }
    },
    stay: () => setPendingHref(null),
  };
}
