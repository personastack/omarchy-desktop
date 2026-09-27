import assert from "node:assert/strict";
import test from "node:test";
import type { DesktopControlState } from "./companion-client.js";
import { applyTrayActionResult, applyTrayStateRead, beginTrayControlAction, beginTrayStateRead, isCurrentTrayControlAction, sameTrayControlSnapshot, trayControlAction, trayControlCanDisconnect, trayControlCanRepair, trayControlCanSetUp, trayControlStatus } from "./tray-control.js";

const baseState: DesktopControlState = {
  installation_id: "install_01",
  operating_system: "linux",
  runtime_available: true,
  cua_ready: true,
  cua_upgrade_required: false,
  native_executor_ready: true,
  gateway_connected: true,
  relay_active: true,
  relay_paused: false,
  user_paused: false,
};

test("tray actions distinguish user pause and allow pausing a degraded active relay", () => {
  assert.equal(trayControlAction({ state: baseState }), "pause");
  assert.equal(trayControlAction({ state: { ...baseState, user_paused: true, relay_paused: true }, stateFresh: true }), "resume");
  assert.equal(trayControlAction({ state: { ...baseState, user_paused: true, relay_active: false } }), undefined);
  assert.equal(trayControlAction({ state: { ...baseState, relay_active: false, gateway_connected: false } }), undefined);
  assert.equal(trayControlAction({ state: { ...baseState, relay_paused: true } }), "pause");
  assert.equal(trayControlAction({ state: { ...baseState, runtime_available: false } }), undefined);
  assert.equal(trayControlAction({ state: { ...baseState, installation_id: null, relay_paused: true } }), undefined);
  assert.equal(trayControlCanSetUp({ state: { ...baseState, installation_id: null } }), true);
  assert.equal(trayControlCanSetUp({ state: { ...baseState, installation_id: null, cua_upgrade_required: true } }), false);
  assert.equal(trayControlCanSetUp({ state: { ...baseState, installation_id: null, runtime_available: false } }), false);
  assert.equal(trayControlCanDisconnect({ state: baseState }), true);
  assert.equal(trayControlCanRepair({ state: baseState }), true);
  assert.equal(trayControlCanRepair({ state: { ...baseState, installation_id: null } }), false);
  assert.equal(trayControlCanDisconnect({ state: { ...baseState, installation_id: null } }), false);
  assert.equal(trayControlCanDisconnect({ state: { ...baseState, runtime_available: false } }), false);
  assert.equal(trayControlAction({ state: { ...baseState, cua_ready: false } }), "pause");
  assert.equal(trayControlAction({ state: { ...baseState, native_executor_ready: false } }), "pause");
  assert.equal(trayControlAction({ state: { ...baseState, gateway_connected: false } }), "pause");
  assert.equal(trayControlAction({ state: { ...baseState, relay_active: undefined } }), "pause");
  assert.equal(trayControlAction({ state: { ...baseState, user_paused: true, relay_paused: true, gateway_connected: false }, stateFresh: true }), "resume");
  assert.equal(trayControlStatus({ state: { ...baseState, relay_paused: true } }), "Desktop Control: Remote control is not active");
  assert.equal(trayControlStatus({ state: { ...baseState, relay_active: undefined } }), "Desktop Control: Relay status unavailable");
  assert.equal(trayControlStatus({ state: { ...baseState, runtime_available: false, installation_id: null, relay_paused: true } }), "Desktop Control: Native runtime unavailable");
  assert.equal(trayControlStatus({ state: { ...baseState, cua_ready: false, cua_upgrade_required: true } }), "Desktop Control: App update required");
});

test("a successful state refresh does not erase a failed lifecycle action", () => {
  const failed = applyTrayActionResult({}, { ok: false, error: "session_locked" });
  assert.equal(trayControlAction({ ...failed, state: { ...baseState, user_paused: true, relay_paused: true } }), undefined);
  const refreshed = applyTrayStateRead(failed, { ok: true, ...baseState, relay_paused: true, user_paused: true });
  assert.equal(refreshed.actionError, "session_locked");
  assert.equal(trayControlAction(refreshed), "resume");
  assert.equal(trayControlStatus(refreshed), "Desktop Control: Needs attention");
  assert.equal(sameTrayControlSnapshot(failed, refreshed), false);
});

test("failed state refresh disables actions from stale state until a fresh read recovers", () => {
  const stale = applyTrayStateRead({ state: baseState }, { ok: false, error: "unavailable" });
  assert.equal(stale.state, baseState);
  assert.equal(trayControlAction(stale), "pause");
  assert.equal(trayControlCanSetUp(applyTrayStateRead({ state: { ...baseState, installation_id: null } }, { ok: false, error: "unavailable" })), false);
  assert.equal(isCurrentTrayControlAction(stale, "pause"), true);
  assert.equal(trayControlStatus(stale), "Desktop Control: Needs attention");
  const recovered = applyTrayStateRead(stale, { ok: true, ...baseState });
  assert.equal(trayControlAction(recovered), "pause");
  assert.equal(isCurrentTrayControlAction(recovered, "pause"), true);
  assert.equal(recovered.refreshError, undefined);
});

test("stale user-paused state hides Resume while stale active sessions still allow Pause", () => {
  const cachedPause = { ...baseState, user_paused: true, relay_paused: true };
  const failed = applyTrayStateRead({ state: cachedPause, stateFresh: true }, { ok: false, error: "unavailable" });
  assert.equal(trayControlAction(failed), undefined);
  assert.equal(trayControlCanDisconnect(failed), true);
  const pending = beginTrayStateRead({ state: cachedPause, stateFresh: true });
  assert.equal(trayControlAction(pending), undefined);
  assert.equal(trayControlAction({ state: baseState, stateFresh: false, refreshError: "unavailable" }), "pause");
});

test("pending state read keeps the last action usable from an already open menu", () => {
  const reading = beginTrayStateRead({ state: baseState, stateFresh: true });
  assert.equal(trayControlAction(reading), "pause");
  assert.equal(isCurrentTrayControlAction(reading, "pause"), true);
});

test("runtime availability changes count as tray state changes", () => {
  const available = { state: baseState };
  const unavailable = { state: { ...baseState, runtime_available: false } };
  assert.equal(sameTrayControlSnapshot(available, unavailable), false);
});

test("Cua incompatibility has a typed update-required tray status", () => {
  const state = { ...baseState, cua_ready: false, cua_upgrade_required: true };
  assert.equal(trayControlStatus({ state }), "Desktop Control: App update required");
  assert.equal(trayControlStatus({ state: { ...state, user_paused: true, relay_paused: true } }), "Desktop Control: App update required");
  assert.equal(trayControlStatus({ state: { ...state, relay_active: false } }), "Desktop Control: App update required");
  assert.equal(trayControlStatus({ state: { ...state, relay_active: undefined } }), "Desktop Control: App update required");
  assert.equal(trayControlStatus({ state: { ...state, installation_id: null } }), "Desktop Control: App update required");
  assert.equal(sameTrayControlSnapshot({ state: baseState }, { state }), false);
});

test("failed repair preserves update-required status after a fresh state read", () => {
  const incompatible = { ...baseState, cua_ready: false, cua_upgrade_required: true };
  const failedRepair = applyTrayActionResult({ state: incompatible, stateFresh: true }, { ok: false, error: "unavailable" });
  assert.equal(failedRepair.actionError, "unavailable");
  assert.equal(trayControlStatus(failedRepair), "Desktop Control: App update required");
  const refreshed = applyTrayStateRead(failedRepair, { ok: true, ...incompatible });
  assert.equal(refreshed.actionError, "unavailable");
  assert.equal(refreshed.stateFresh, true);
  assert.equal(trayControlStatus(refreshed), "Desktop Control: App update required");
});

test("initial state read failure takes precedence over the empty-state message", () => {
  assert.equal(trayControlStatus({ refreshError: "unavailable" }), "Desktop Control: Needs attention");
  assert.equal(trayControlStatus({ actionError: "unavailable" }), "Desktop Control: Needs attention");
});

test("a successful action clears prior errors and records the authoritative state", () => {
  const failed = applyTrayActionResult({ refreshError: "unavailable", actionError: "session_locked" }, { ok: true, ...baseState, relay_paused: true, user_paused: true });
  assert.equal(failed.actionError, undefined);
  assert.equal(failed.refreshError, undefined);
  assert.equal(failed.state?.user_paused, true);
});

test("action dispatch keeps the last action available while the native menu disables it", () => {
  const pending = beginTrayControlAction({ state: baseState, stateFresh: true, actionError: "unavailable" });
  assert.equal(trayControlAction(pending), "pause");
  assert.equal(pending.actionError, "unavailable");
});

test("native runtime failure takes precedence over a missing installation", () => {
  assert.equal(trayControlStatus({ state: { ...baseState, installation_id: null, runtime_available: false } }), "Desktop Control: Native runtime unavailable");
});
