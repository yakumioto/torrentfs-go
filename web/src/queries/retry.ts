import { ApiError } from '../api/errors';

export function shouldRetry(failureCount: number, error: unknown): boolean {
  if (error instanceof ApiError && error.clientError) {
    return false;
  }
  return failureCount < 3;
}

export function retryDelay(attemptIndex: number): number {
  return Math.min(1000 * 2 ** attemptIndex, 8000);
}
