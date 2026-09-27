import { createHash, randomBytes } from "node:crypto";

import { parseDesktopAuthHandoffCallback } from "./security.js";

const handoffLifetimeMs = 10 * 60_000;

export interface PendingDesktopAuthHandoff {
  readonly attemptId: string;
  readonly verifier: string;
  readonly expiresAt: number;
}

export interface DesktopAuthHandoffLaunch {
  readonly attempt: PendingDesktopAuthHandoff;
  readonly url: string;
}

export interface AcceptedDesktopAuthHandoff {
  readonly attemptId: string;
  readonly code: string;
  readonly verifier: string;
}

type SessionFetch = (input: string, init: RequestInit) => Promise<Response>;

export function beginDesktopAuthHandoff(appOrigin: URL, now: number): DesktopAuthHandoffLaunch {
  const attemptId = randomBytes(32).toString("base64url");
  const verifier = randomBytes(32).toString("base64url");
  const challenge = createHash("sha256").update(verifier).digest("base64url");
  const startURL = new URL("/auth/desktop-handoff/start", appOrigin);
  startURL.searchParams.set("attempt_id", attemptId);
  startURL.searchParams.set("code_challenge", challenge);

  return {
    attempt: { attemptId, verifier, expiresAt: now + handoffLifetimeMs },
    url: startURL.href,
  };
}

export interface DesktopAuthHandoffConsumption {
  readonly pending: PendingDesktopAuthHandoff | undefined;
  readonly callback: AcceptedDesktopAuthHandoff | undefined;
}

export function consumeDesktopAuthHandoff(
  attempt: PendingDesktopAuthHandoff | undefined,
  callbackURL: string,
  now: number,
): DesktopAuthHandoffConsumption {
  if (!attempt) return { pending: undefined, callback: undefined };
  if (attempt.expiresAt <= now) return { pending: undefined, callback: undefined };
  const callback = parseDesktopAuthHandoffCallback(callbackURL, attempt.attemptId);
  if (!callback) return { pending: attempt, callback: undefined };
  return { pending: undefined, callback: { ...callback, verifier: attempt.verifier } };
}

export async function exchangeDesktopAuthHandoff(
  sessionFetch: SessionFetch,
  appOrigin: URL,
  callback: AcceptedDesktopAuthHandoff,
): Promise<void> {
  const exchangeURL = new URL("/auth/desktop-handoff/exchange", appOrigin);
  const response = await sessionFetch(exchangeURL.href, {
    method: "POST",
    credentials: "include",
    redirect: "manual",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    body: JSON.stringify({
      attempt_id: callback.attemptId,
      code: callback.code,
      code_verifier: callback.verifier,
      app_origin: appOrigin.origin,
    }),
  });
  if (!response.ok) throw new Error("desktop sign-in exchange failed");
  let result: unknown;
  try {
    result = await response.json();
  } catch {
    throw new Error("desktop sign-in exchange failed");
  }
  if (result === null || typeof result !== "object" || Array.isArray(result) || Object.keys(result).length !== 1 ||
      (result as Record<string, unknown>).ok !== true) {
    throw new Error("desktop sign-in exchange failed");
  }
}
