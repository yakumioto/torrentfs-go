import { QueryClient } from '@tanstack/react-query';
import { retryDelay, shouldRetry } from '../queries/retry';

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: shouldRetry,
      retryDelay,
      refetchOnWindowFocus: true,
      refetchIntervalInBackground: false,
    },
  },
});
