export const DEFAULT_APP_URL = "https://my.personastack.ai/user/personas";
export const APP_URL_SWITCH = "--personastack-url";
export const BACKGROUND_SWITCH = "--background";

export type BridgeRole = "main" | "chat" | "stack" | "activity";

export interface BridgeFrameIdentity {
  readonly role: BridgeRole | undefined;
  readonly registered: boolean;
  readonly isMainFrame: boolean;
  readonly frameURL: string;
  readonly topFrameURL: string;
  readonly currentGeneration: boolean;
}

export interface BridgeEnvelope {
  readonly generation: number;
  readonly payload: unknown;
}

export type ChatMainCommand =
  | Readonly<{ version: "1"; action: "sync"; scope: string }>
  | Readonly<{ version: "1"; action: "open_persona_chat"; scope: string; persona_id: string }>;

export type ChatWindowCommand = Readonly<{ version: "1"; action: "minimize" | "close" | "collapse" | "expand" | "pin" }>
  | Readonly<{ version: "1"; action: "drag"; dx: number; dy: number }>;

export type StackCommand =
  | Readonly<{ version: "1"; action: "open_stack_view"; stack_id: string; view: "graph" | "stream" }>
  | Readonly<{ version: "1"; action: "open_persona_activity"; persona_id: string }>;

export type DesktopControlCommand =
  | Readonly<{ version: "1"; action: "sync" | "state"; scope: string }>
  | Readonly<{ version: "1"; action: "prepare"; scope: string; enrollment_ticket: string }>;

export type LocalSessionCommand =
  | Readonly<{ version: "1"; action: "state"; scope: string }>
  | Readonly<{ version: "1"; action: "select_harness"; scope: string; harness: "codex" | "claude_code" }>
  | Readonly<{ version: "1"; action: "prepare"; scope: string; persona_id: string; harness: "codex" | "claude_code" }>
  | Readonly<{ version: "1"; action: "configure"; scope: string; pending_id: string; bundle: Readonly<Record<string, unknown>> }>;

export function resolveAppURL(args: readonly string[], packagedDefault?: string): URL {
  const switchIndex = args.indexOf(APP_URL_SWITCH);
  const override = switchIndex >= 0 ? args[switchIndex + 1] : undefined;
  return parseHTTPURL(override) ?? parseHTTPURL(packagedDefault) ?? new URL(DEFAULT_APP_URL);
}

export function shouldStartInBackground(args: readonly string[]): boolean {
  return args.includes(BACKGROUND_SWITCH);
}

export function parseHTTPURL(value: unknown): URL | undefined {
  if (typeof value !== "string" || value.trim() === "") return undefined;
  try {
    const url = new URL(value);
    if (!new Set(["http:", "https:"]).has(url.protocol) || url.hostname === "" || url.username !== "" || url.password !== "") {
      return undefined;
    }
    return url;
  } catch {
    return undefined;
  }
}

export function isTrustedAppURL(value: string, appURL: URL): boolean {
  const candidate = parseHTTPURL(value);
  return candidate !== undefined && candidate.origin === appURL.origin;
}

export function isInternalPersonaStackURL(value: string, appURL: URL): boolean {
  const candidate = parseHTTPURL(value);
  if (!candidate) return false;
  if (candidate.origin === appURL.origin) return true;
  const host = candidate.hostname.toLowerCase();
  return candidate.protocol === "https:" &&
    (host === "my.personastack.ai" || host === "personastack.ai" || host === "personastack.ericgreer.info" || host.endsWith(".personastack.ai"));
}

export function authorizeBridgeFrame(identity: BridgeFrameIdentity, appURL: URL): boolean {
  return identity.registered && identity.role !== undefined && identity.isMainFrame && identity.currentGeneration &&
    isTrustedAppURL(identity.frameURL, appURL) && isTrustedAppURL(identity.topFrameURL, appURL);
}

export function isCurrentBridgeGeneration(received: unknown, current: number): received is number {
  return typeof received === "number" && Number.isSafeInteger(received) && received === current;
}

export function unwrapBridgePayload(value: unknown): BridgeEnvelope | undefined {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return undefined;
  const envelope = value as Record<string, unknown>;
  if (Object.keys(envelope).length !== 2 || !Object.hasOwn(envelope, "generation") || !Object.hasOwn(envelope, "payload") ||
      typeof envelope.generation !== "number" || !Number.isSafeInteger(envelope.generation)) return undefined;
  return { generation: envelope.generation, payload: envelope.payload };
}

export function parseChatMainCommand(value: unknown): ChatMainCommand | undefined {
  if (!isRecord(value) || value.version !== "1" || typeof value.action !== "string") return undefined;
  if (value.action === "sync" && hasExactKeys(value, ["version", "action", "scope"]) && isBoundedText(value.scope, 512)) {
    return { version: "1", action: "sync", scope: value.scope };
  }
  if (value.action === "open_persona_chat" && hasExactKeys(value, ["version", "action", "scope", "persona_id"]) &&
      isBoundedText(value.scope, 512) && value.scope !== "" && isValidID(value.persona_id)) {
    return { version: "1", action: "open_persona_chat", scope: value.scope, persona_id: value.persona_id };
  }
  return undefined;
}

export function parseChatWindowCommand(value: unknown): ChatWindowCommand | undefined {
  if (!isRecord(value) || value.version !== "1" || typeof value.action !== "string") return undefined;
  if (value.action === "drag" && hasExactKeys(value, ["version", "action", "dx", "dy"]) && isBoundedNumber(value.dx) && isBoundedNumber(value.dy)) {
    return { version: "1", action: "drag", dx: value.dx, dy: value.dy };
  }
  const actions = new Set<string>(["minimize", "close", "collapse", "expand", "pin"]);
  if (actions.has(value.action as "minimize" | "close" | "collapse" | "expand" | "pin") && hasExactKeys(value, ["version", "action"])) {
    return { version: "1", action: value.action as "minimize" | "close" | "collapse" | "expand" | "pin" };
  }
  return undefined;
}

export function parseStackCommand(value: unknown): StackCommand | undefined {
  if (!isRecord(value) || value.version !== "1" || typeof value.action !== "string") return undefined;
  if (value.action === "open_stack_view" && hasExactKeys(value, ["version", "action", "stack_id", "view"]) &&
      isValidID(value.stack_id) && (value.view === "graph" || value.view === "stream")) {
    return { version: "1", action: "open_stack_view", stack_id: value.stack_id, view: value.view };
  }
  if (value.action === "open_persona_activity" && hasExactKeys(value, ["version", "action", "persona_id"]) && isValidID(value.persona_id)) {
    return { version: "1", action: "open_persona_activity", persona_id: value.persona_id };
  }
  return undefined;
}

export function parseDesktopControlCommand(value: unknown): DesktopControlCommand | undefined {
  if (!isRecord(value) || value.version !== "1" || typeof value.action !== "string") return undefined;
  if ((value.action === "sync" || value.action === "state") && hasExactKeys(value, ["version", "action", "scope"]) &&
      isBoundedUTF8Text(value.scope, 512) && value.scope.trim() === value.scope &&
      value.scope !== "desktop:lifecycle") {
    return { version: "1", action: value.action, scope: value.scope };
  }
  if (value.action === "prepare" && hasExactKeys(value, ["version", "action", "scope", "enrollment_ticket"]) &&
      isBoundedUTF8Text(value.scope, 512) && value.scope !== "" && value.scope.trim() === value.scope &&
      value.scope !== "desktop:lifecycle" &&
      typeof value.enrollment_ticket === "string" && /^[A-Za-z0-9_-]{43}$/.test(value.enrollment_ticket)) {
    return { version: "1", action: "prepare", scope: value.scope, enrollment_ticket: value.enrollment_ticket };
  }
  return undefined;
}

export function parseLocalSessionCommand(value: unknown): LocalSessionCommand | undefined {
  if (!isRecord(value) || value.version !== "1" || typeof value.action !== "string" ||
      !isBoundedUTF8Text(value.scope, 512) || value.scope.trim() !== value.scope) return undefined;
  if (value.action === "state" && hasExactKeys(value, ["version", "action", "scope"])) {
    return { version: "1", action: "state", scope: value.scope };
  }
  if (value.action === "select_harness" && hasExactKeys(value, ["version", "action", "scope", "harness"]) && isHarness(value.harness) && value.scope !== "") {
    return { version: "1", action: "select_harness", scope: value.scope, harness: value.harness };
  }
  if (value.action === "prepare" && hasExactKeys(value, ["version", "action", "scope", "persona_id", "harness"]) &&
      isHarness(value.harness) && value.scope !== "" && isValidID(value.persona_id)) {
    return { version: "1", action: "prepare", scope: value.scope, persona_id: value.persona_id, harness: value.harness };
  }
  if (value.action === "configure" && hasExactKeys(value, ["version", "action", "scope", "pending_id", "bundle"]) &&
      value.scope !== "" && typeof value.pending_id === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value.pending_id) &&
      isRecord(value.bundle) && hasBoundedJSONSize(value.bundle, 12 * 1024 * 1024)) {
    return { version: "1", action: "configure", scope: value.scope, pending_id: value.pending_id, bundle: value.bundle };
  }
  return undefined;
}

export function isNewConcernEvent(value: unknown): boolean {
  return isRecord(value) && hasExactKeys(value, ["version", "event"]) && value.version === "1" && value.event === "created";
}

export function isSafeExternalURL(value: unknown): value is string {
  const url = parseHTTPURL(value);
  return url !== undefined;
}

export function isGoogleOAuthURL(value: unknown): value is string {
  const url = parseHTTPURL(value);
  return url !== undefined && url.protocol === "https:" && url.hostname.toLowerCase() === "accounts.google.com";
}

export function isAllowedMainNavigation(value: string, appURL: URL): boolean {
  return isInternalPersonaStackURL(value, appURL) || isGoogleOAuthURL(value);
}

export function isAllowedOAuthPopupURL(value: unknown, appURL: URL): value is string {
  return isGoogleOAuthURL(value) || (typeof value === "string" && isTrustedAppURL(value, appURL));
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function hasExactKeys(value: Record<string, unknown>, keys: readonly string[]): boolean {
  return Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key));
}

function isBoundedText(value: unknown, maxLength: number): value is string {
  return typeof value === "string" && value.length <= maxLength;
}

function isBoundedUTF8Text(value: unknown, maxBytes: number): value is string {
  return typeof value === "string" && new TextEncoder().encode(value).byteLength <= maxBytes;
}

function isValidID(value: unknown): value is string {
  return typeof value === "string" && value.length > 0 && value.length <= 128 && /^[a-zA-Z0-9_-]+$/.test(value);
}

function isBoundedNumber(value: unknown): value is number {
  return typeof value === "number" && Number.isFinite(value) && Math.abs(value) <= 10_000;
}

function isHarness(value: unknown): value is "codex" | "claude_code" {
  return value === "codex" || value === "claude_code";
}

function hasBoundedJSONSize(value: object, maxBytes: number): boolean {
  try {
    return new TextEncoder().encode(JSON.stringify(value)).byteLength <= maxBytes;
  } catch {
    return false;
  }
}
