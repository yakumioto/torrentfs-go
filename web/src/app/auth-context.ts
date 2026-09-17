import { createContext, useContext } from 'react';
import type { ApiClient } from '../api/client';

export type AuthPhase = 'probing' | 'anonymous' | 'authenticated' | 'login' | 'error';

export interface AuthContextValue {
  api: ApiClient;
  phase: AuthPhase;
  isReady: boolean;
  isAuthenticated: boolean;
  loginError: string;
  sessionNotice: string;
  connectionError: string;
  loginExpiresIn?: number;
  login: (username: string, password: string) => Promise<boolean>;
  logout: () => Promise<void>;
  retryProbe: () => void;
}

export const AuthContext = createContext<AuthContextValue | null>(null);

export function useAuth(): AuthContextValue {
  const value = useContext(AuthContext);
  if (value === null) {
    throw new Error('useAuth must be used within AuthProvider');
  }
  return value;
}
