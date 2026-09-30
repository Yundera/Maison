// Thin typed wrapper over fetch for the REST API.

async function req<T>(
  method: string,
  path: string,
  body?: unknown,
  signal?: AbortSignal,
): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
    signal,
  })
  if (!res.ok) throw await apiError(method, path, res)
  if (res.status === 204) return undefined as T
  const ct = res.headers.get('content-type') ?? ''
  return (ct.includes('application/json') ? await res.json() : await res.text()) as T
}

/**
 * The error to throw for a failed response.
 *
 * The API answers a failure with `{"error": "<message>"}`, and those messages are
 * written for the operator to read ("line 3: … is not KEY=VALUE"). Callers put
 * `e.message` straight on screen, so surface that alone rather than wrapping it in
 * the request line — which is for the console, not the user.
 *
 * Anything without such a message — a proxy's HTML error page, an empty body — has
 * nothing to show, so there we name the request that failed instead.
 */
async function apiError(method: string, path: string, res: Response): Promise<Error> {
  const text = await res.text().catch(() => '')
  let body: Record<string, unknown> | null = null
  try {
    body = JSON.parse(text) as Record<string, unknown>
  } catch {
    /* not JSON — fall through */
  }
  const msg = typeof body?.error === 'string' ? body.error : ''
  return new ApiError(msg || `${method} ${path} -> ${res.status} ${text}`.trim(), res.status, body)
}

/** A failed response. `message` is what to show; `body` is the parsed JSON body, for
 *  a caller that acts on a flag the API sets beside the message (an update refused
 *  with `no_rollback`, say). */
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly body: Record<string, unknown> | null,
  ) {
    super(message)
  }
}

export const api = {
  // The signal is here for reads a user can walk away from: abandoning the request
  // also cancels the work behind it, which for a backup lookup is a container running
  // against a repository for an answer nobody will read.
  get: <T>(path: string, signal?: AbortSignal) => req<T>('GET', path, undefined, signal),
  post: <T>(path: string, body?: unknown) => req<T>('POST', path, body),
  put: <T>(path: string, body?: unknown) => req<T>('PUT', path, body),
  del: <T>(path: string, body?: unknown) => req<T>('DELETE', path, body),
}
