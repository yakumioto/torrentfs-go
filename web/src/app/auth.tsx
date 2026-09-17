import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { ApiClient } from '../api/client';
import { ApiError, errorMessage } from '../api/errors';
import { queryKeys } from '../queries/keys';
import { sortTorrents } from '../queries/sort';
import { AuthContext, type AuthContextValue, type AuthPhase } from './auth-context';

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const queryClient = useQueryClient();
  const tokenStore = useMemo(() => ({ token: undefined as string | undefined }), []);
  const probeControllerRef = useRef<AbortController | undefined>(undefined);
  const [accessToken, setAccessToken] = useState<string | undefined>(undefined);
  const [phase, setPhase] = useState<AuthPhase>('probing');
  const [loginError, setLoginError] = useState('');
  const [connectionError, setConnectionError] = useState('');
  const [loginExpiresIn, setLoginExpiresIn] = useState<number | undefined>(undefined);

  const clearServerState = useCallback(async () => {
    await queryClient.cancelQueries();
    queryClient.clear();
  }, [queryClient]);

  const handleUnauthorized = useCallback(() => {
    tokenStore.token = undefined;
    setAccessToken(undefined);
    setPhase('login');
    setLoginError('');
    void clearServerState();
  }, [clearServerState, tokenStore]);

  const api = useMemo(() => new ApiClient({
    getToken: () => tokenStore.token,
    onUnauthorized: handleUnauthorized,
  }), [handleUnauthorized, tokenStore]);

  const executeProbe = useCallback(async (controller: AbortController) => {
    try {
      const torrents = await api.listTorrents(controller.signal);
      if (controller.signal.aborted) {
        return;
      }
      queryClient.setQueryData(queryKeys.torrents, sortTorrents(torrents));
      setPhase(tokenStore.token === undefined ? 'anonymous' : 'authenticated');
    } catch (error) {
      if (controller.signal.aborted) {
        return;
      }
      if (error instanceof ApiError && error.status === 401) {
        setPhase('login');
        return;
      }
      setConnectionError(errorMessage(error, 'The daemon is not reachable.'));
      setPhase('error');
    }
  }, [api, queryClient, tokenStore]);

  const probe = useCallback(() => {
    probeControllerRef.current?.abort();
    const controller = new AbortController();
    probeControllerRef.current = controller;
    setPhase('probing');
    setConnectionError('');
    setLoginError('');
    void executeProbe(controller);
  }, [executeProbe]);

  useEffect(() => {
    const controller = new AbortController();
    probeControllerRef.current = controller;
    void executeProbe(controller);
    return () => controller.abort();
  }, [executeProbe]);

  const login = useCallback(async (username: string, password: string): Promise<boolean> => {
    setLoginError('');
    try {
      const response = await api.login(username, password);
      tokenStore.token = response.token;
      setAccessToken(response.token);
      setLoginExpiresIn(response.expires_in);
      setPhase('authenticated');
      await queryClient.invalidateQueries({ queryKey: queryKeys.torrents });
      return true;
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        setLoginError('Invalid username or password.');
      } else {
        setLoginError(errorMessage(error));
      }
      return false;
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
      tokenStore.token = undefined;
      setAccessToken(undefined);
      setLoginExpiresIn(undefined);
      await clearServerState();
      void probe();
    }
  }, [api, clearServerState, probe, tokenStore]);

  const value: AuthContextValue = {
    api,
    phase,
    isReady: phase === 'anonymous' || phase === 'authenticated',
    isAuthenticated: phase === 'authenticated' && accessToken !== undefined,
    loginError,
    connectionError,
    loginExpiresIn,
    login,
    logout,
    retryProbe: () => void probe(),
  };

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}
