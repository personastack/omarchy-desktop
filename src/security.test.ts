import assert from "node:assert/strict";
import test from "node:test";

import {
  APP_URL_SWITCH,
  BACKGROUND_SWITCH,
  authorizeBridgeFrame,
  handleFrameNavigation,
  isAllowedUserExternalLink,
  isSafeNewContextURL,
  isEnterpriseOIDCStartURL,
  isTrustedPermissionRequest,
  shouldKeepPersonaStackLinkInApp,
  isAllowedMainNavigation,
  isGoogleOAuthURL,
  isGoogleOAuthAuthorizationURL,
  googleIntegrationOAuthStartState,
  isNewConcernEvent,
  isInternalPersonaStackURL,
  isTrustedAppURL,
  isCurrentBridgeGeneration,
  parseDesktopControlCommand,
  parseLocalSessionCommand,
  parseChatMainCommand,
	parseChatWindowCommand,
	parseDesktopAuthHandoffCallback,
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

test("new-context links allow HTTP, HTTPS, and mailto while rejecting unsafe schemes", () => {
  assert.equal(isSafeNewContextURL("https://personastack.ai/privacy"), true);
  assert.equal(isSafeNewContextURL("http://docs.example.test/"), true);
  assert.equal(isSafeNewContextURL("mailto:support@personastack.ai"), true);
  assert.equal(isSafeNewContextURL("mailto:support@personastack.ai?subject=Help"), true);
  assert.equal(isSafeNewContextURL("mailto:support@personastack.ai#fragment"), false);
  assert.equal(isSafeNewContextURL("mailto:personastack.ai"), false);
  assert.equal(isSafeNewContextURL("javascript:alert(1)"), false);
  assert.equal(isSafeNewContextURL("file:///etc/passwd"), false);
  assert.equal(isSafeNewContextURL("mailto:support@personastack.ai%0d%0aBcc:evil@example.test"), false);
});

test("Google Services OAuth external URL requires the API-issued state and hosted callback", () => {
  const appURL = new URL("https://my.personastack.ai/user/google-services");
  const state = "a".repeat(64);
  const authorizationURL = new URL("https://accounts.google.com/o/oauth2/v2/auth");
  authorizationURL.searchParams.set("client_id", "client-id");
  authorizationURL.searchParams.set("redirect_uri", "https://my.personastack.ai/callbacks/integrations/google/oauth/callback");
  authorizationURL.searchParams.set("response_type", "code");
  authorizationURL.searchParams.set("scope", "openid email");
  authorizationURL.searchParams.set("state", state);

  assert.equal(googleIntegrationOAuthStartState(authorizationURL.href, appURL), state);
  assert.equal(isGoogleOAuthAuthorizationURL(authorizationURL.href), true);
  assert.equal(isGoogleOAuthAuthorizationURL("https://accounts.google.com/gsi/select"), false);
  assert.equal(googleIntegrationOAuthStartState(
    authorizationURL.href.replace(encodeURIComponent("https://my.personastack.ai/callbacks/integrations/google/oauth/callback"), encodeURIComponent("https://attacker.example/callback")), appURL,
  ), undefined);
  assert.equal(googleIntegrationOAuthStartState(
    authorizationURL.href.replace("state=" + state, "state=" + state + "&state=" + state), appURL,
  ), undefined);
  assert.equal(googleIntegrationOAuthStartState("https://accounts.example.com/o/oauth2/v2/auth", appURL), undefined);
  let prevented = false;
  assert.equal(handleFrameNavigation(
    { preventDefault: () => { prevented = true; } }, authorizationURL.href, "main", false,
    appURL, () => {},
  ), false);
  assert.equal(prevented, true);
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
  assert.equal(authorizeBridgeFrame({ ...identity, frameURL: "https://sso.example.com/", topFrameURL: "https://sso.example.com/" }, appURL), false);
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
	assert.equal(isAllowedMainNavigation("https://accounts.google.com/o/oauth2/auth", appURL), false);
  assert.equal(isAllowedMainNavigation("https://attacker.example/", appURL), false);
  assert.equal(isEnterpriseOIDCStartURL("https://my.personastack.ai/auth/oidc/start", appURL), true);
  assert.equal(isEnterpriseOIDCStartURL("https://my.personastack.ai/auth/oidc/start/extra", appURL), false);
  assert.equal(isEnterpriseOIDCStartURL("https://attacker.example/auth/oidc/start", appURL), false);
  assert.equal(isAllowedMainNavigation("https://sso.example.com/authorize", appURL), false);
  const handoffAttempt = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA";
  const handoffCode = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB";
  assert.deepEqual(parseDesktopAuthHandoffCallback(`personastack://auth/callback?attempt_id=${handoffAttempt}&code=${handoffCode}`, handoffAttempt), {
    attemptId: handoffAttempt,
    code: handoffCode,
  });
  assert.equal(parseDesktopAuthHandoffCallback(`personastack://auth/callback?attempt_id=${handoffAttempt}&code=${handoffCode}&token=secret`, handoffAttempt), undefined);
  assert.equal(parseDesktopAuthHandoffCallback(`personastack://auth/callback?attempt_id=${handoffAttempt}&code=${handoffCode}`, handoffAttempt.replace("A", "C")), undefined);
  assert.equal(parseDesktopAuthHandoffCallback(`personastack://auth:444/callback?attempt_id=${handoffAttempt}&code=${handoffCode}`, handoffAttempt), undefined);
  assert.equal(isTrustedPermissionRequest("main", true, "https://my.personastack.ai/login", undefined, appURL), true);
  assert.equal(isTrustedPermissionRequest("main", false, "https://my.personastack.ai/login", undefined, appURL), false);
  assert.equal(isTrustedPermissionRequest("chat", true, "https://my.personastack.ai/login", undefined, appURL), false);
  assert.equal(isTrustedPermissionRequest("main", true, "https://sso.example.com/login", undefined, appURL), false);
  assert.equal(isTrustedPermissionRequest("main", true, "https://my.personastack.ai/login", "https://sso.example.com", appURL), false);
  assert.equal(shouldKeepPersonaStackLinkInApp("https://sso.example.com/continue", appURL), false);
  assert.equal(shouldKeepPersonaStackLinkInApp("https://my.personastack.ai/login", appURL), true);
  assert.equal(shouldKeepPersonaStackLinkInApp("https://my.personastack.ai/user/personas", appURL), true);
  assert.equal(isAllowedUserExternalLink("main", true, true, "https://sso.example.com/login", "https://docs.example.com/", appURL), true);
  assert.equal(isAllowedUserExternalLink("main", true, true, "https://sso.example.com/login", "http://docs.example.com/", appURL), true);
  assert.equal(isAllowedUserExternalLink("main", true, true, "https://my.personastack.ai/login", "https://docs.example.com/", appURL), false);
  assert.equal(isAllowedUserExternalLink("main", true, false, "https://sso.example.com/login", "https://docs.example.com/", appURL), false);
  assert.equal(isAllowedUserExternalLink("chat", true, true, "https://sso.example.com/login", "https://docs.example.com/", appURL), false);
  assert.equal(isAllowedUserExternalLink("main", false, true, "https://sso.example.com/login", "https://docs.example.com/", appURL), false);
  assert.equal(isAllowedUserExternalLink("main", true, true, "https://sso.example.com/login", "javascript:alert(1)", appURL), false);
  assert.equal(isAllowedUserExternalLink("main", true, true, "https://sso.example.com/login", "https://my.personastack.ai/login", appURL), true);
  let prevented = false;
  let popoutsInvalidated = false;
  const navigationEvent = { preventDefault: () => { prevented = true; } };
  const frameNavigationAllowed = handleFrameNavigation(
    navigationEvent,
    "https://attacker.example/frame",
    "main",
    false,
    appURL,
    () => { popoutsInvalidated = true; },
  );
  assert.equal(frameNavigationAllowed, false);
  assert.equal(prevented, true);
  assert.equal(popoutsInvalidated, false);
  prevented = false;
  const googleSignInFrameAllowed = handleFrameNavigation(
    navigationEvent,
    "https://accounts.google.com/gsi/iframe/select?client_id=example",
    "main",
    false,
    appURL,
    () => { popoutsInvalidated = true; },
  );
  assert.equal(googleSignInFrameAllowed, true);
  assert.equal(prevented, false);
  const idpFrameBlocked = handleFrameNavigation(
    navigationEvent,
    "https://sso.example.com/frame/login",
    "main",
    false,
    appURL,
    () => { popoutsInvalidated = true; },
  );
  assert.equal(idpFrameBlocked, false);
  const idpCrossOriginFrameBlocked = handleFrameNavigation(
    navigationEvent,
    "https://frames.attacker.example/frame",
    "main",
    false,
    appURL,
    () => { popoutsInvalidated = true; },
  );
  assert.equal(idpCrossOriginFrameBlocked, false);
  prevented = false;
  const topLevelExternalAllowed = handleFrameNavigation(
    navigationEvent,
    "https://accounts.google.com/o/oauth2/auth",
    "main",
    true,
    appURL,
    () => { popoutsInvalidated = true; },
  );
  assert.equal(topLevelExternalAllowed, false);
  assert.equal(prevented, true);
  assert.equal(popoutsInvalidated, true);
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

test("chat window bridge accepts each finite native action and rejects extra fields", () => {
  for (const action of ["minimize", "close", "collapse", "expand", "pin"] as const) {
    assert.deepEqual(parseChatWindowCommand({ version: "1", action }), { version: "1", action });
    assert.equal(parseChatWindowCommand({ version: "1", action, extra: true }), undefined);
  }
  assert.deepEqual(parseChatWindowCommand({ version: "1", action: "drag", dx: -8, dy: 4 }), {
    version: "1", action: "drag", dx: -8, dy: 4,
  });
  assert.deepEqual(parseChatWindowCommand({ version: "1", action: "drag", dx: 0.5, dy: -0.25 }), {
    version: "1", action: "drag", dx: 0.5, dy: -0.25,
  });
  assert.equal(parseChatWindowCommand({ version: "1", action: "drag", dx: 1, dy: 2, extra: true }), undefined);
  assert.equal(parseChatWindowCommand({ version: "1", action: "toggle_pin" }), undefined);
});

test("Desktop Control bridge accepts only hosted lifecycle commands", () => {
  const ticket = "A".repeat(43);
  assert.deepEqual(parseDesktopControlCommand({ version: "1", action: "sync", scope: "" }), { version: "1", action: "sync", scope: "" });
  assert.deepEqual(parseDesktopControlCommand({ version: "1", action: "state", scope: "" }), { version: "1", action: "state", scope: "" });
  assert.equal(parseDesktopControlCommand({ version: "1", action: "pause", scope: "workspace:request" }), undefined);
  assert.equal(parseDesktopControlCommand({ version: "1", action: "resume", scope: "workspace:request" }), undefined);
  assert.equal(parseDesktopControlCommand({ version: "1", action: "disconnect", scope: "desktop:lifecycle" }), undefined);
  assert.equal(parseDesktopControlCommand({ version: "1", action: "state", scope: "desktop:lifecycle" }), undefined);
  assert.equal(parseDesktopControlCommand({ version: "1", action: "sync", scope: "desktop:lifecycle" }), undefined);
  assert.equal(parseDesktopControlCommand({ version: "1", action: "prepare", scope: "desktop:lifecycle", enrollment_ticket: ticket }), undefined);
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
