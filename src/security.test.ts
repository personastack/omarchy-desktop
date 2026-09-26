import assert from "node:assert/strict";
import test from "node:test";

import {
  APP_URL_SWITCH,
  BACKGROUND_SWITCH,
  authorizeBridgeFrame,
  isAllowedOAuthPopupURL,
  isAllowedMainNavigation,
  isGoogleOAuthURL,
  isNewConcernEvent,
  isInternalPersonaStackURL,
  isTrustedAppURL,
  isCurrentBridgeGeneration,
  parseDesktopControlCommand,
  parseLocalSessionCommand,
  parseChatMainCommand,
  parseChatWindowCommand,
  parseStackCommand,
  resolveAppURL,
  shouldStartInBackground,
  unwrapBridgePayload,
} from "./security.js";

test("URL resolution preserves packaged defaults and rejects invalid overrides", () => {
  assert.equal(resolveAppURL([APP_URL_SWITCH, "https://personastack.ericgreer.info"]).origin, "https://personastack.ericgreer.info");
  assert.equal(resolveAppURL([APP_URL_SWITCH, "file:///tmp/page"], "https://example.test/app").href, "https://example.test/app");
  assert.equal(resolveAppURL([APP_URL_SWITCH], "bad").href, "https://my.personastack.ai/user/personas");
  assert.equal(resolveAppURL([APP_URL_SWITCH, "https://user:pass@example.test"], "https://example.test").origin, "https://example.test");
});

test("background launch is explicit and leaves ordinary launches visible", () => {
  assert.equal(shouldStartInBackground(["/usr/bin/personastack"]), false);
  assert.equal(shouldStartInBackground(["/usr/bin/personastack", BACKGROUND_SWITCH]), true);
  assert.equal(shouldStartInBackground(["/usr/bin/personastack", "--not-background"]), false);
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

test("Desktop Control bridge accepts only hosted lifecycle commands", () => {
  const ticket = "A".repeat(43);
  assert.deepEqual(parseDesktopControlCommand({ version: "1", action: "sync", scope: "" }), { version: "1", action: "sync", scope: "" });
  assert.deepEqual(parseDesktopControlCommand({ version: "1", action: "state", scope: "" }), { version: "1", action: "state", scope: "" });
  assert.deepEqual(parseDesktopControlCommand({ version: "1", action: "pause", scope: "workspace:request" }), { version: "1", action: "pause", scope: "workspace:request" });
  assert.deepEqual(parseDesktopControlCommand({ version: "1", action: "resume", scope: "workspace:request" }), { version: "1", action: "resume", scope: "workspace:request" });
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

test("local-session bridge admits bounded versioned commands only", () => {
  assert.deepEqual(parseLocalSessionCommand({ version: "1", action: "state", scope: "" }), {
    version: "1", action: "state", scope: "",
  });
  assert.deepEqual(parseLocalSessionCommand({ version: "1", action: "select_harness", scope: "scope-a", harness: "codex" }), {
    version: "1", action: "select_harness", scope: "scope-a", harness: "codex",
  });
  assert.deepEqual(parseLocalSessionCommand({ version: "1", action: "prepare", scope: "scope-a", persona_id: "persona_1", harness: "claude_code" }), {
    version: "1", action: "prepare", scope: "scope-a", persona_id: "persona_1", harness: "claude_code",
  });
  assert.equal(parseLocalSessionCommand({ version: "1", action: "prepare", scope: "scope-a", persona_id: "../x", harness: "codex" }), undefined);
  assert.equal(parseLocalSessionCommand({ version: "1", action: "select_harness", scope: "scope-a", harness: "other" }), undefined);
  assert.equal(parseLocalSessionCommand({ version: "1", action: "state", scope: "scope-a", extra: true }), undefined);
  assert.deepEqual(parseLocalSessionCommand({ version: "1", action: "configure", scope: "scope-a", pending_id: "c725451f-2d11-4e46-adfd-e92f2fc84c01", bundle: { bounded: true } }), {
    version: "1", action: "configure", scope: "scope-a", pending_id: "c725451f-2d11-4e46-adfd-e92f2fc84c01", bundle: { bounded: true },
  });
  assert.equal(parseLocalSessionCommand({ version: "1", action: "configure", scope: "scope-a", pending_id: "bad", bundle: {} }), undefined);
  assert.equal(parseLocalSessionCommand({ version: "1", action: "state", scope: "é".repeat(257) }), undefined);
});

test("concern notification accepts only the generic new-concern event", () => {
  assert.equal(isNewConcernEvent({ version: "1", event: "created" }), true);
  assert.equal(isNewConcernEvent({ version: "1", event: "created", title: "private" }), false);
  assert.equal(isNewConcernEvent({ version: "1", event: "updated" }), false);
});
