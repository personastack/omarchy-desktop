import assert from "node:assert/strict";
import test from "node:test";

import { documentNavigationEffects } from "./document-navigation.js";

test("main document navigation cancels page-owned work while preserving same-scope popouts", () => {
  assert.deepEqual(documentNavigationEffects("main", "navigation-started"), {
    cancelPageRequests: true,
    invalidatePopouts: false,
    closeWindow: false,
  });
  assert.deepEqual(documentNavigationEffects("main", "response-received", "https://my.personastack.ai/user/personas", 200), {
    cancelPageRequests: false,
    invalidatePopouts: false,
    closeWindow: false,
  });
});

test("main HTTP errors and authentication routes fence the page and all popouts", () => {
  for (const responseCode of [401, 403, 500]) {
    assert.deepEqual(documentNavigationEffects("main", "response-received", "https://my.personastack.ai/user/personas", responseCode), {
      cancelPageRequests: true,
      invalidatePopouts: true,
      closeWindow: false,
    });
  }
  for (const path of ["/login", "/logout"]) {
    assert.equal(documentNavigationEffects("main", "response-received", `https://my.personastack.ai${path}`, 200).invalidatePopouts, true);
  }
});

test("popout login redirects, HTTP errors, denied navigation and renderer loss close that window", () => {
  for (const [event, url, responseCode] of [
    ["response-received", "https://my.personastack.ai/login", 200],
    ["response-received", "https://my.personastack.ai/chat", 401],
    ["response-received", "https://my.personastack.ai/chat", 500],
    ["navigation-denied", "", 0],
    ["load-failed", "", 0],
    ["renderer-gone", "", 0],
  ] as const) {
    assert.equal(documentNavigationEffects("chat", event, url, responseCode).closeWindow, true);
  }
});

test("main failures and renderer loss clear the companion scope and popouts", () => {
  for (const event of ["load-failed", "renderer-gone"] as const) {
    assert.deepEqual(documentNavigationEffects("main", event), {
      cancelPageRequests: true,
      invalidatePopouts: true,
      closeWindow: false,
    });
  }
});
