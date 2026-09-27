import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";

import { beginDesktopAuthHandoff, consumeDesktopAuthHandoff, exchangeDesktopAuthHandoff } from "./desktop-auth-handoff.js";

const now = 1_800_000_000_000;

test("desktop sign-in builds a system-browser URL with a PKCE challenge", () => {
  const appOrigin = new URL("https://my.personastack.ai/user/personas");
  const launch = beginDesktopAuthHandoff(appOrigin, now);
  const url = new URL(launch.url);
  const expectedChallenge = createHash("sha256").update(launch.attempt.verifier).digest("base64url");

  assert.equal(url.origin, appOrigin.origin);
  assert.equal(url.pathname, "/auth/desktop-handoff/start");
  assert.equal(url.searchParams.get("attempt_id"), launch.attempt.attemptId);
  assert.equal(url.searchParams.get("code_challenge"), expectedChallenge);
  assert.equal(url.searchParams.has("code_verifier"), false);
  assert.equal(url.searchParams.has("session_token"), false);
  assert.equal(launch.attempt.expiresAt, now + 10 * 60_000);
  assert.match(launch.attempt.attemptId, /^[A-Za-z0-9_-]{43}$/);
  assert.match(launch.attempt.verifier, /^[A-Za-z0-9_-]{43}$/);
});

test("desktop sign-in consumes only a matching callback within its lifetime", () => {
  const launch = beginDesktopAuthHandoff(new URL("https://my.personastack.ai"), now);
  const code = "B".repeat(43);
  const callback = `personastack://auth/callback?attempt_id=${launch.attempt.attemptId}&code=${code}`;

  const accepted = consumeDesktopAuthHandoff(launch.attempt, callback, launch.attempt.expiresAt - 1);
  assert.deepEqual(accepted, {
    pending: undefined,
    callback: { attemptId: launch.attempt.attemptId, code, verifier: launch.attempt.verifier },
  });
  assert.deepEqual(consumeDesktopAuthHandoff(accepted.pending, callback, now), { pending: undefined, callback: undefined });
  assert.deepEqual(consumeDesktopAuthHandoff(launch.attempt, callback, launch.attempt.expiresAt), { pending: undefined, callback: undefined });
  assert.deepEqual(consumeDesktopAuthHandoff(launch.attempt, callback.replace(launch.attempt.attemptId, "A".repeat(43)), now), {
    pending: launch.attempt,
    callback: undefined,
  });
  assert.deepEqual(consumeDesktopAuthHandoff(launch.attempt, callback.replace("auth/callback", "auth:444/callback"), now), {
    pending: launch.attempt,
    callback: undefined,
  });
  assert.deepEqual(consumeDesktopAuthHandoff(launch.attempt, `${callback}&session_token=secret`, now), {
    pending: launch.attempt,
    callback: undefined,
  });
});

test("desktop sign-in exchange uses only the persistent session and bounded callback values", async () => {
  const launch = beginDesktopAuthHandoff(new URL("https://my.personastack.ai"), now);
  const callback = {
    attemptId: launch.attempt.attemptId,
    code: "B".repeat(43),
    verifier: launch.attempt.verifier,
  };
  const requests: Array<{ input: string; init: RequestInit }> = [];

  const result = await exchangeDesktopAuthHandoff(async (input, init) => {
    requests.push({ input, init });
    return new Response(JSON.stringify({ ok: true }), { status: 200, headers: { "Content-Type": "application/json" } });
  }, new URL("https://my.personastack.ai/user/personas"), callback);

  assert.equal(result, undefined);
  assert.equal(requests.length, 1);
  assert.equal(requests[0]?.input, "https://my.personastack.ai/auth/desktop-handoff/exchange");
  assert.equal(new URL(requests[0]?.input ?? "https://invalid").search, "");
  assert.equal(requests[0]?.init.method, "POST");
  assert.equal(requests[0]?.init.credentials, "include");
  assert.equal(requests[0]?.init.redirect, "manual");
  assert.deepEqual(requests[0]?.init.headers, { "Content-Type": "application/json", Accept: "application/json" });
  assert.deepEqual(JSON.parse(String(requests[0]?.init.body)), {
    attempt_id: callback.attemptId,
    code: callback.code,
    code_verifier: callback.verifier,
    app_origin: "https://my.personastack.ai",
  });
});

test("desktop sign-in exchange rejects HTTP and malformed hosted responses", async () => {
  const callback = { attemptId: "A".repeat(43), code: "B".repeat(43), verifier: "C".repeat(43) };
  const appOrigin = new URL("https://my.personastack.ai");
  const badResponses = [
    new Response("{}", { status: 401 }),
    new Response("not-json", { status: 200 }),
    new Response(JSON.stringify({ ok: false }), { status: 200 }),
    new Response(JSON.stringify({ ok: true, error: "unexpected" }), { status: 200 }),
  ];

  for (const response of badResponses) {
    await assert.rejects(exchangeDesktopAuthHandoff(async () => response, appOrigin, callback), /desktop sign-in exchange failed/);
  }
});
