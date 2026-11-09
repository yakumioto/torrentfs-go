export class ApiError extends Error {
  readonly status: number;
  readonly serverMessage: string;
  readonly wwwAuthenticate: string;
  /**
   * Stable machine-readable reason for a rejected request. Clients branch on
   * this rather than on server text, which is display-only.
   */
  readonly code: string;
  /**
   * The decoded response body when the error response carried one. Structured
   * failures use it to carry the per-field details a display message cannot.
   */
  readonly payload: unknown;

  constructor(status: number, message: string, wwwAuthenticate = '', code = '', payload?: unknown) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.serverMessage = message;
    this.wwwAuthenticate = wwwAuthenticate;
    this.code = code;
    this.payload = payload;
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
  let code = '';
  let parsed: unknown;
  if (body !== '') {
    try {
      parsed = JSON.parse(body);
      if (typeof parsed === 'object' && parsed !== null) {
        if ('error' in parsed) {
          const error = parsed.error;
          if (typeof error === 'string' && error !== '') {
            message = error;
          }
        }
        if ('code' in parsed) {
          const value = parsed.code;
          if (typeof value === 'string') {
            code = value;
          }
        }
      }
    } catch {
      message = body;
    }
  }
  return new ApiError(response.status, message, response.headers.get('WWW-Authenticate') ?? '', code, parsed);
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
