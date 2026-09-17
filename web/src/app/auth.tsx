import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { ApiClient } from '../api/client';
import { ApiError, errorMessage } from '../api/errors';
import { queryKeys } from '../queries/keys';
import { sortTorrents } from '../queries/sort';
import { AuthContext, type AuthContextValue, type AuthPhase } from './auth-context';

const SESSION_NOTICE = 'Your session expired or the page was refreshed. Sign in again to continue.';

type AuthAction = 'idle' | 'logging-in';

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const queryClient = useQueryClient();
  const tokenStore = useMemo(() => ({ token: undefined as string | undefined }), []);
  const probeControllerRef = useRef<AbortController | undefined>(undefined);
  const authRevisionRef = useRef(0);
  const authActionRef = useRef<AuthAction>('idle');
  const [accessToken, setAccessToken] = useState<string | undefined>(undefined);
  const [phase, setPhase] = useState<AuthPhase>('probing');
  const [loginError, setLoginError] = useState('');
  const [sessionNotice, setSessionNotice] = useState('');
  const [connectionError, setConnectionError] = useState('');
  const [loginExpiresIn, setLoginExpiresIn] = useState<number | undefined>(undefined);

  const clearServerState = useCallback(async (revision?: number) => {
    await queryClient.cancelQueries();
    if (revision === undefined || revision === authRevisionRef.current) {
      queryClient.clear();
    }
  }, [queryClient]);

  const handleUnauthorized = useCallback((requestToken?: string, requestSignal?: AbortSignal | null, requestRevision?: number) => {
    if (requestSignal?.aborted || (requestRevision !== undefined && requestRevision !== authRevisionRef.current) || authActionRef.current === 'logging-in' || requestToken !== tokenStore.token) {
      return;
    }
    const revision = ++authRevisionRef.current;
    probeControllerRef.current?.abort();
    tokenStore.token = undefined;
    setAccessToken(undefined);
    setPhase('login');
    setLoginError('');
    setSessionNotice(SESSION_NOTICE);
    setConnectionError('');
    setLoginExpiresIn(undefined);
    void clearServerState(revision);
  }, [clearServerState, tokenStore]);

  const api = useMemo(() => new ApiClient({
    getToken: () => tokenStore.token,
    getAuthRevision: () => authRevisionRef.current,
    onUnauthorized: handleUnauthorized,
  }), [handleUnauthorized, tokenStore]);

  const executeProbe = useCallback(async (controller: AbortController, revision: number) => {
    try {
      const torrents = await api.listTorrents(controller.signal);
      if (controller.signal.aborted || revision !== authRevisionRef.current || probeControllerRef.current !== controller) {
        return;
      }
      queryClient.setQueryData(queryKeys.torrents, sortTorrents(torrents));
      setSessionNotice('');
      setConnectionError('');
      setPhase(tokenStore.token === undefined ? 'anonymous' : 'authenticated');
    } catch (error) {
      if (controller.signal.aborted || revision !== authRevisionRef.current || probeControllerRef.current !== controller) {
        return;
      }
      if (error instanceof ApiError && error.status === 401) {
        return;
      }
      setConnectionError(errorMessage(error, 'The daemon is not reachable.'));
      setPhase('error');
    }
  }, [api, queryClient, tokenStore]);

  const probe = useCallback(() => {
    const revision = ++authRevisionRef.current;
    probeControllerRef.current?.abort();
    const controller = new AbortController();
    probeControllerRef.current = controller;
    setPhase('probing');
    setConnectionError('');
    setLoginError('');
    setSessionNotice('');
    void executeProbe(controller, revision);
  }, [executeProbe]);

  useEffect(() => {
    const revision = ++authRevisionRef.current;
    const controller = new AbortController();
    probeControllerRef.current = controller;
    void executeProbe(controller, revision);
    return () => controller.abort();
  }, [executeProbe]);

  const login = useCallback(async (username: string, password: string): Promise<boolean> => {
    const revision = ++authRevisionRef.current;
    authActionRef.current = 'logging-in';
    probeControllerRef.current?.abort();
    setLoginError('');
    setSessionNotice('');
    setConnectionError('');
    try {
      const response = await api.login(username, password);
      if (revision !== authRevisionRef.current) {
        return false;
      }
      tokenStore.token = response.token;
      setAccessToken(response.token);
      setLoginExpiresIn(response.expires_in);
      setPhase('authenticated');
      setSessionNotice('');
      authActionRef.current = 'idle';
      await queryClient.invalidateQueries({ queryKey: queryKeys.torrents });
      return revision === authRevisionRef.current;
    } catch (error) {
      if (revision !== authRevisionRef.current) {
        return false;
      }
      if (error instanceof ApiError && error.status === 401) {
        setLoginError('Invalid username or password.');
      } else {
        setLoginError(errorMessage(error));
      }
      return false;
    } finally {
      if (revision === authRevisionRef.current) {
        authActionRef.current = 'idle';
      }
    }
  }, [api, queryClient, tokenStore]);

  const logout = useCallback(async () => {
    const token = tokenStore.token;
    try {
      if (token !== undefined) {
        await api.logout(token);
      }
    } catch {
      // Local logout still wins when the daemon cannot revoke the token.
    } finally {
      const revision = ++authRevisionRef.current;
      probeControllerRef.current?.abort();
      authActionRef.current = 'idle';
      tokenStore.token = undefined;
      setAccessToken(undefined);
      setLoginExpiresIn(undefined);
      setSessionNotice('');
      setConnectionError('');
      await clearServerState(revision);
      void probe();
    }
  }, [api, clearServerState, probe, tokenStore]);

  const value: AuthContextValue = {
    api,
    phase,
    isReady: phase === 'anonymous' || phase === 'authenticated',
    isAuthenticated: phase === 'authenticated' && accessToken !== undefined,
    loginError,
    sessionNotice,
    connectionError,
    loginExpiresIn,
    login,
    logout,
    retryProbe: probe,
  };

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}
