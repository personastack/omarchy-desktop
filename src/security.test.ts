import assert from "node:assert/strict";
import test from "node:test";

import {
  APP_URL_SWITCH,
  authorizeBridgeFrame,
  isAllowedOAuthPopupURL,
  isAllowedMainNavigation,
  isGoogleOAuthURL,
  isNewConcernEvent,
  isInternalPersonaStackURL,
  isTrustedAppURL,
  isCurrentBridgeGeneration,
  parseDesktopControlCommand,
  parseChatMainCommand,
  parseChatWindowCommand,
  parseStackCommand,
  resolveAppURL,
  unwrapBridgePayload,
} from "./security.js";

test("URL resolution preserves packaged defaults and rejects invalid overrides", () => {
  assert.equal(resolveAppURL([APP_URL_SWITCH, "https://personastack.ericgreer.info"]).origin, "https://personastack.ericgreer.info");
  assert.equal(resolveAppURL([APP_URL_SWITCH, "file:///tmp/page"], "https://example.test/app").href, "https://example.test/app");
  assert.equal(resolveAppURL([APP_URL_SWITCH], "bad").href, "https://my.personastack.ai/user/personas");
  assert.equal(resolveAppURL([APP_URL_SWITCH, "https://user:pass@example.test"], "https://example.test").origin, "https://example.test");
});

test("bridge admits only registered current main frames at the exact configured origin", () => {
  const appURL = new URL("https://my.personastack.ai/user/personas");
  const identity = {
    role: "main" as const,
    registered: true,
    isMainFrame: true,
    frameURL: "https://my.personastack.ai/user/personas",
    topFrameURL: "https://my.personastack.ai/user/personas",
    currentGeneration: true,
  };
  assert.equal(authorizeBridgeFrame(identity, appURL), true);
  assert.equal(authorizeBridgeFrame({ ...identity, frameURL: "https://attacker.my.personastack.ai/" }, appURL), false);
  assert.equal(authorizeBridgeFrame({ ...identity, isMainFrame: false }, appURL), false);
  assert.equal(authorizeBridgeFrame({ ...identity, registered: false }, appURL), false);
  assert.equal(authorizeBridgeFrame({ ...identity, currentGeneration: false }, appURL), false);
  assert.equal(isCurrentBridgeGeneration(7, 7), true);
  assert.equal(isCurrentBridgeGeneration(6, 7), false);
  assert.equal(isCurrentBridgeGeneration("7", 7), false);
  assert.deepEqual(unwrapBridgePayload({ generation: 7, payload: { action: "sync" } }), { generation: 7, payload: { action: "sync" } });
  assert.equal(unwrapBridgePayload({ generation: 7, payload: null, extra: true }), undefined);
  assert.equal(isTrustedAppURL("https://my.personastack.ai:443/another-path", appURL), true);
  assert.equal(isTrustedAppURL("http://my.personastack.ai/", appURL), false);
  assert.equal(isInternalPersonaStackURL("https://personastack.ai/", appURL), true);
  assert.equal(isInternalPersonaStackURL("https://example.org/", appURL), false);
  assert.equal(isGoogleOAuthURL("https://accounts.google.com/o/oauth2/auth"), true);
  assert.equal(isGoogleOAuthURL("https://google.com/o/oauth2/auth"), false);
  assert.equal(isAllowedMainNavigation("https://accounts.google.com/o/oauth2/auth", appURL), true);
  assert.equal(isAllowedMainNavigation("https://attacker.example/", appURL), false);
  assert.equal(isAllowedOAuthPopupURL("https://accounts.google.com/o/oauth2/auth", appURL), true);
  assert.equal(isAllowedOAuthPopupURL("https://my.personastack.ai/auth/callback", appURL), true);
  assert.equal(isAllowedOAuthPopupURL("https://attacker.example/callback", appURL), false);
});

test("chat and stack bridges accept only their finite exact payloads", () => {
  assert.deepEqual(parseChatMainCommand({ version: "1", action: "open_persona_chat", scope: "scope-1", persona_id: "persona_1" }), {
    version: "1", action: "open_persona_chat", scope: "scope-1", persona_id: "persona_1",
  });
  assert.equal(parseChatMainCommand({ version: "1", action: "sync", scope: "", extra: true }), undefined);
  assert.equal(parseChatMainCommand({ version: "1", action: "open_persona_chat", scope: "", persona_id: "p" }), undefined);
  assert.deepEqual(parseChatWindowCommand({ version: "1", action: "drag", dx: 8, dy: -4 }), { version: "1", action: "drag", dx: 8, dy: -4 });
  assert.equal(parseChatWindowCommand({ version: "1", action: "drag", dx: true, dy: 2 }), undefined);
  assert.deepEqual(parseStackCommand({ version: "1", action: "open_stack_view", stack_id: "stack-1", view: "graph" }), {
    version: "1", action: "open_stack_view", stack_id: "stack-1", view: "graph",
  });
  assert.equal(parseStackCommand({ version: "1", action: "open_stack_view", stack_id: "../etc", view: "graph" }), undefined);
});

test("Desktop Control bridge accepts only hosted sync, state, and prepare commands", () => {
  const ticket = "A".repeat(43);
  assert.deepEqual(parseDesktopControlCommand({ version: "1", action: "sync", scope: "" }), { version: "1", action: "sync", scope: "" });
  assert.deepEqual(parseDesktopControlCommand({ version: "1", action: "state", scope: "" }), { version: "1", action: "state", scope: "" });
  assert.deepEqual(parseDesktopControlCommand({ version: "1", action: "prepare", scope: "workspace:request", enrollment_ticket: ticket }),
    { version: "1", action: "prepare", scope: "workspace:request", enrollment_ticket: ticket });
  assert.equal(parseDesktopControlCommand({ version: "1", action: "prepare", scope: "x", enrollment_ticket: "short" }), undefined);
  assert.equal(parseDesktopControlCommand({ version: "1", action: "prepare", scope: "", enrollment_ticket: ticket }), undefined);
  assert.equal(parseDesktopControlCommand({ version: "1", action: "state", scope: "", enrollment_ticket: ticket }), undefined);
  assert.equal(parseDesktopControlCommand({ version: "1", action: "sync", scope: " padded " }), undefined);
  assert.equal(parseDesktopControlCommand({ version: "1", action: "sync", scope: "é".repeat(300) }), undefined);
  assert.equal(parseDesktopControlCommand({ version: "1", action: "status" }), undefined);
  assert.equal(parseDesktopControlCommand({ version: "1", action: "prepare", scope: "", enrollment_ticket: ticket, extra: true }), undefined);
});

test("concern notification accepts only the generic new-concern event", () => {
  assert.equal(isNewConcernEvent({ version: "1", event: "created" }), true);
  assert.equal(isNewConcernEvent({ version: "1", event: "created", title: "private" }), false);
  assert.equal(isNewConcernEvent({ version: "1", event: "updated" }), false);
});
