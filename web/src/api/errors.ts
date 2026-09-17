export class ApiError extends Error {
  readonly status: number;
  readonly serverMessage: string;
  readonly wwwAuthenticate: string;

  constructor(status: number, message: string, wwwAuthenticate = '') {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.serverMessage = message;
    this.wwwAuthenticate = wwwAuthenticate;
  }

  get unauthorized(): boolean {
    return this.status === 401;
  }

  get clientError(): boolean {
    return this.status >= 400 && this.status < 500;
  }
}

export async function apiErrorFromResponse(response: Response): Promise<ApiError> {
  const body = await response.text();
  let message = response.statusText || 'Request failed';
  if (body !== '') {
    try {
      const parsed: unknown = JSON.parse(body);
      if (typeof parsed === 'object' && parsed !== null && 'error' in parsed) {
        const error = parsed.error;
        if (typeof error === 'string' && error !== '') {
          message = error;
        }
      }
    } catch {
      message = body;
    }
  }
  return new ApiError(response.status, message, response.headers.get('WWW-Authenticate') ?? '');
}

export function errorMessage(error: unknown, fallback = 'The daemon could not complete that request.'): string {
  if (error instanceof ApiError) {
    return error.serverMessage;
  }
  if (error instanceof Error && error.message !== '') {
    return error.message;
  }
  return fallback;
}
