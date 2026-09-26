import type { DesktopControlError, DesktopControlResult, DesktopControlState } from "./companion-client.js";

export type TrayControlSnapshot = Readonly<{
  state?: DesktopControlState;
  stateFresh?: boolean;
  refreshError?: DesktopControlError;
  actionError?: DesktopControlError;
}>;

export function trayControlAction(snapshot: TrayControlSnapshot): "pause" | "resume" | undefined {
  const state = snapshot.state;
  if (!state?.installation_id || !state.runtime_available) return undefined;
  if (state.user_paused) {
    return snapshot.stateFresh === true && !snapshot.refreshError && state.relay_active === true ? "resume" : undefined;
  }
  return state.relay_active === true || state.gateway_connected ? "pause" : undefined;
}

export function isCurrentTrayControlAction(snapshot: TrayControlSnapshot, action: "pause" | "resume"): boolean {
  return trayControlAction(snapshot) === action;
}

export function trayControlCanSetUp(snapshot: TrayControlSnapshot): boolean {
  const state = snapshot.state;
  return !snapshot.refreshError && snapshot.stateFresh !== false && !!state?.runtime_available && !state.installation_id;
}

export function trayControlCanDisconnect(snapshot: TrayControlSnapshot): boolean {
  const state = snapshot.state;
  return !!state?.installation_id && !!state.runtime_available;
}

export function trayControlStatus(snapshot: TrayControlSnapshot): string {
  const state = snapshot.state;
  if (snapshot.actionError || snapshot.refreshError) return "Desktop Control: Needs attention";
  if (!state) return "Desktop Control: Checking status…";
  if (!state.runtime_available) return "Desktop Control: Native runtime unavailable";
  if (!state.installation_id) return "Desktop Control: Not set up";
  if (state.user_paused) return "Desktop Control: Remote control paused";
  if (state.relay_active === undefined) return "Desktop Control: Relay status unavailable";
  if (!state.relay_active || state.relay_paused) return "Desktop Control: Remote control is not active";
  if (!state.cua_ready) return "Desktop Control: Cua service unavailable";
  if (!state.native_executor_ready) return "Desktop Control: Waiting for an unlocked session";
  if (!state.gateway_connected) return "Desktop Control: Connecting";
  return "Desktop Control: Connected";
}

export function applyTrayStateRead(snapshot: TrayControlSnapshot, result: DesktopControlResult): TrayControlSnapshot {
  if (!result.ok) return { ...snapshot, stateFresh: false, refreshError: result.error };
  if (!("installation_id" in result)) return snapshot;
  return { ...snapshot, state: result, stateFresh: true, refreshError: undefined };
}

export function beginTrayStateRead(snapshot: TrayControlSnapshot): TrayControlSnapshot {
  return { ...snapshot, stateFresh: false };
}

export function beginTrayControlAction(snapshot: TrayControlSnapshot): TrayControlSnapshot {
  return { ...snapshot, stateFresh: false, refreshError: undefined };
}

export function applyTrayActionResult(snapshot: TrayControlSnapshot, result: DesktopControlResult): TrayControlSnapshot {
  if (!result.ok) return { ...snapshot, stateFresh: false, actionError: result.error };
  if (!("installation_id" in result)) return { ...snapshot, stateFresh: false, actionError: "unavailable" };
  return { state: result, stateFresh: true };
}

export function sameTrayControlSnapshot(left: TrayControlSnapshot, right: TrayControlSnapshot): boolean {
  return sameState(left.state, right.state) && left.stateFresh === right.stateFresh &&
    left.refreshError === right.refreshError && left.actionError === right.actionError;
}

function sameState(left: DesktopControlState | undefined, right: DesktopControlState | undefined): boolean {
  if (!left || !right) return left === right;
  return left.installation_id === right.installation_id && left.cua_ready === right.cua_ready &&
    left.native_executor_ready === right.native_executor_ready && left.gateway_connected === right.gateway_connected &&
    left.relay_active === right.relay_active && left.relay_paused === right.relay_paused && left.user_paused === right.user_paused &&
    left.runtime_available === right.runtime_available;
}
