export interface PendingExternalOAuthReturn {
  readonly attemptId: number;
  readonly returnURL: string;
  readonly expiresAt: number;
  readonly blurred: boolean;
}

export interface ExternalOAuthFocusResult {
  readonly pending: PendingExternalOAuthReturn | undefined;
  readonly returnURL?: string;
}

export function beginExternalOAuthReturn(
  attemptId: number,
  returnURL: string,
  expiresAt: number,
): PendingExternalOAuthReturn {
  return { attemptId, returnURL, expiresAt, blurred: false };
}

export function markExternalOAuthBlurred(
  pending: PendingExternalOAuthReturn,
): PendingExternalOAuthReturn {
  return { ...pending, blurred: true };
}

export function refreshExternalOAuthOnFocus(
  pending: PendingExternalOAuthReturn | undefined,
  currentURL: string,
  now: number,
): ExternalOAuthFocusResult {
  if (!pending) return { pending: undefined };
  if (pending.expiresAt <= now || currentURL !== pending.returnURL) return { pending: undefined };
  if (!pending.blurred) return { pending };
  return { pending: undefined, returnURL: pending.returnURL };
}

export function isCurrentExternalOAuthAttempt(
  pending: PendingExternalOAuthReturn | undefined,
  attemptId: number,
  currentURL: string,
): boolean {
  return pending?.attemptId === attemptId && pending.returnURL === currentURL;
}
