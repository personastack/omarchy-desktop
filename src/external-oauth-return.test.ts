import assert from "node:assert/strict";
import test from "node:test";

import {
  beginExternalOAuthReturn,
  isCurrentExternalOAuthAttempt,
  markExternalOAuthBlurred,
  refreshExternalOAuthOnFocus,
} from "./external-oauth-return.js";

test("a new Google Services attempt replaces a canceled attempt", () => {
  const oldAttempt = beginExternalOAuthReturn(1, "https://my.personastack.ai/user/google-services", 600_000);
  const retry = beginExternalOAuthReturn(2, oldAttempt.returnURL, 600_000);

  assert.equal(isCurrentExternalOAuthAttempt(oldAttempt, 1, oldAttempt.returnURL), true);
  assert.equal(isCurrentExternalOAuthAttempt(retry, 1, retry.returnURL), false);
  assert.equal(isCurrentExternalOAuthAttempt(retry, 2, retry.returnURL), true);
});

test("a quick browser return refreshes the hosted page", () => {
  const route = "https://my.personastack.ai/user/google-services";
  const pending = markExternalOAuthBlurred(beginExternalOAuthReturn(1, route, 600_000));

  assert.deepEqual(refreshExternalOAuthOnFocus(pending, route, 1_001), {
    pending: undefined,
    returnURL: route,
  });
});

test("the first app return consumes the refresh so later focus does not reload the page", () => {
  const route = "https://my.personastack.ai/user/google-services";
  const pending = beginExternalOAuthReturn(1, route, 600_000);
  const firstBlur = markExternalOAuthBlurred(pending);
  const earlyReturn = refreshExternalOAuthOnFocus(firstBlur, route, 2_000);

  assert.equal(earlyReturn.returnURL, route);
  assert.equal(earlyReturn.pending, undefined);
  assert.equal(refreshExternalOAuthOnFocus(earlyReturn.pending, route, 2_001).returnURL, undefined);
});

test("a stale browser launch cannot refresh a route the user left", () => {
  const route = "https://my.personastack.ai/user/google-services";
  const pending = beginExternalOAuthReturn(1, route, 600_000);

  assert.equal(isCurrentExternalOAuthAttempt(pending, 1, "https://my.personastack.ai/user/personas"), false);
  assert.deepEqual(refreshExternalOAuthOnFocus(pending, "https://my.personastack.ai/user/personas", 1_000), {
    pending: undefined,
  });
});

test("an expired attempt is discarded", () => {
  const route = "https://my.personastack.ai/user/google-services";
  const pending = beginExternalOAuthReturn(1, route, 10_000);

  assert.deepEqual(refreshExternalOAuthOnFocus(pending, route, 10_000), { pending: undefined });
});
