import type { BridgeRole } from "./security.js";

export type DocumentNavigationEvent = "navigation-started" | "navigation-denied" | "response-received" | "load-failed" | "renderer-gone";

export type DocumentNavigationEffects = Readonly<{
  cancelPageRequests: boolean;
  invalidatePopouts: boolean;
  closeWindow: boolean;
}>;

export function documentNavigationEffects(
  role: BridgeRole,
  event: DocumentNavigationEvent,
  url = "",
  responseCode = 0,
): DocumentNavigationEffects {
  if (event === "navigation-started") {
    return { cancelPageRequests: role === "main", invalidatePopouts: false, closeWindow: false };
  }
  if (event === "navigation-denied") {
    return { cancelPageRequests: false, invalidatePopouts: role === "main", closeWindow: role !== "main" };
  }
  if (event === "load-failed" || event === "renderer-gone") {
    return { cancelPageRequests: role === "main", invalidatePopouts: role === "main", closeWindow: role !== "main" };
  }

  const pathname = pathFor(url);
  const invalidated = responseCode >= 400 || pathname === "/login" || pathname === "/logout";
  return {
    cancelPageRequests: role === "main" && invalidated,
    invalidatePopouts: role === "main" && invalidated,
    closeWindow: role !== "main" && invalidated,
  };
}

function pathFor(value: string): string {
  try {
    return new URL(value).pathname;
  } catch {
    return "";
  }
}
