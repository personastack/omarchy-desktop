import type { ChildProcess } from "node:child_process";

import type { DesktopControlCommand, LocalSessionCommand } from "./security.js";

const MAX_RESPONSE_BYTES = 4 * 1024;
const MAX_REQUEST_BYTES = 13 * 1024 * 1024;
const MAX_PENDING_REQUESTS = 8;
const REQUEST_TIMEOUT_MS = 45_000;
const DESKTOP_CONTROL_PREPARE_TIMEOUT_MS = 4 * 60_000;
// The Go resume operation is bounded to eleven minutes; allow two minutes for
// the typed response and companion shutdown to finish before closing the pipe.
const DESKTOP_CONTROL_RESUME_TIMEOUT_MS = 13 * 60_000;
const LOCAL_SESSION_TIMEOUT_MS = 4 * 60_000;
const MAX_REQUEST_ID = Number.MAX_SAFE_INTEGER;

export type DesktopControlState = Readonly<{
  installation_id: string | null;
  operating_system: "linux";
  runtime_available: boolean;
  cua_ready: boolean;
  native_executor_ready: boolean;
  gateway_connected: boolean;
  relay_active?: boolean;
  relay_paused: boolean;
  user_paused: boolean;
}>;

export type LifecycleAction = "state" | "pause" | "resume";

type DesktopControlPrepared = DesktopControlState;

export type DesktopControlError =
  | "invalid_request"
  | "not_enrolled"
  | "keyring_unavailable"
  | "rejected"
  | "session_locked"
  | "session_state_unknown"
  | "unavailable"
  | "unsupported_platform"
  | "stale_request";

export type DesktopControlResult =
  | Readonly<{ ok: true }>
  | (Readonly<{ ok: true }> & DesktopControlState)
  | (Readonly<{ ok: true }> & DesktopControlPrepared)
  | Readonly<{ ok: false; error: DesktopControlError }>;

export type LocalSessionResult =
  | Readonly<{ ok: true }>
  | Readonly<{ ok: true; version: "2"; harness?: "codex" | "claude_code" }>
  | Readonly<{ ok: true; pending_id: string }>
  | Readonly<{ ok: false; error: LocalSessionError }>;

export type LocalSessionError =
  | "invalid_request"
  | "invalid_bundle"
  | "stale_request"
  | "missing_harness"
  | "outdated_harness"
  | "unsafe_files"
  | "unavailable";

type CompanionResult = DesktopControlResult | LocalSessionResult;
type CompanionAction = DesktopControlCommand["action"] | LifecycleAction | "local_session";

type PendingRequest = Readonly<{
  action: CompanionAction;
  localAction?: LocalSessionCommand["action"];
  resolve: (result: CompanionResult) => void;
  timeout: NodeJS.Timeout;
}>;

export class CompanionClient {
  private readonly pending = new Map<number, PendingRequest>();
  private output = "";
  private nextRequestID = 1;
  private closed = false;
  private closePromise?: Promise<void>;
  private readonly child: ChildProcess;

  constructor(child: ChildProcess) {
    if (!child.stdin || !child.stdout) throw new Error("companion requires piped standard input and output");
    this.child = child;
    child.stdout.setEncoding("utf8");
    child.stdout.on("data", this.onData);
    child.once("error", this.onFailure);
    child.once("exit", this.onFailure);
  }

  get isOpen(): boolean {
    return !this.closed;
  }

  request(command: DesktopControlCommand): Promise<DesktopControlResult> {
    return this.send(command.action, command) as Promise<DesktopControlResult>;
  }

  requestLifecycle(action: LifecycleAction): Promise<DesktopControlResult> {
    return this.send(action, { version: "1", action, scope: "desktop:lifecycle" }) as Promise<DesktopControlResult>;
  }

  requestLocalSession(command: LocalSessionCommand): Promise<LocalSessionResult> {
    return this.send("local_session", {
      version: "1",
      action: "local_session",
      scope: command.scope,
      local_session: command,
    }, command.action) as Promise<LocalSessionResult>;
  }

  private send(action: CompanionAction, payload: object, localAction?: LocalSessionCommand["action"]): Promise<CompanionResult> {
    if (this.closed || this.pending.size >= MAX_PENDING_REQUESTS || this.nextRequestID > MAX_REQUEST_ID) {
      return Promise.resolve({ ok: false, error: "unavailable" });
    }

    const id = this.nextRequestID++;
    const request = { id, ...payload };
    let encoded: string | undefined;
    try {
      encoded = JSON.stringify(request);
    } catch {
      return Promise.resolve({ ok: false, error: "unavailable" });
    }
    if (encoded === undefined || Buffer.byteLength(encoded) > MAX_REQUEST_BYTES) {
      return Promise.resolve({ ok: false, error: "unavailable" });
    }
    return new Promise((resolve) => {
      const timeoutMs = action === "prepare"
        ? DESKTOP_CONTROL_PREPARE_TIMEOUT_MS
        : action === "resume" ? DESKTOP_CONTROL_RESUME_TIMEOUT_MS
          : localAction === "prepare" || localAction === "configure" ? LOCAL_SESSION_TIMEOUT_MS : REQUEST_TIMEOUT_MS;
      const timeout = setTimeout(() => this.failAll(), timeoutMs);
      this.pending.set(id, { action, localAction, resolve, timeout });
      try {
        this.child.stdin?.write(`${encoded}\n`, (error) => {
          if (error) this.failAll();
        });
      } catch {
        this.failAll();
      }
    });
  }

  close(): Promise<void> {
    if (this.closePromise) return this.closePromise;
    this.closed = true;
    for (const pending of this.pending.values()) {
      clearTimeout(pending.timeout);
      pending.resolve({ ok: false, error: "unavailable" });
    }
    this.pending.clear();
    this.closePromise = new Promise((resolve) => {
      if (this.child.exitCode !== null || this.child.signalCode !== null) {
        resolve();
        return;
      }
      this.child.once("exit", () => resolve());
      this.child.once("close", () => resolve());
      this.child.once("error", () => resolve());
      this.child.stdin?.end();
    });
    return this.closePromise;
  }

  private readonly onData = (chunk: Buffer | string): void => {
    this.output += Buffer.isBuffer(chunk) ? chunk.toString("utf8") : chunk;
    let newline = this.output.indexOf("\n");
    while (newline >= 0) {
      const line = this.output.slice(0, newline);
      this.output = this.output.slice(newline + 1);
      if (Buffer.byteLength(line) > MAX_RESPONSE_BYTES || !this.acceptLine(line)) {
        this.failAll();
        return;
      }
      newline = this.output.indexOf("\n");
    }
    if (Buffer.byteLength(this.output) > MAX_RESPONSE_BYTES) this.failAll();
  };

  private readonly onFailure = (): void => {
    this.failAll();
  };

  private acceptLine(line: string): boolean {
    let value: unknown;
    try {
      value = JSON.parse(line) as unknown;
    } catch {
      return false;
    }
    const pendingByID = isRecord(value) && Number.isSafeInteger(value.id) ? this.pending.get(Number(value.id)) : undefined;
    if (!pendingByID) return false;
    const response = parseResponse(value, pendingByID.action);
    if (!response) return false;
    const pending = this.pending.get(response.id);
    if (!pending || !matchesCommandResponse(pending.action, pending.localAction, response.result)) return false;
    clearTimeout(pending.timeout);
    this.pending.delete(response.id);
    pending.resolve(response.result);
    return true;
  }

  private failAll(): void {
    if (this.closed) return;
    void this.close();
  }
}

function matchesCommandResponse(action: CompanionAction, localAction: LocalSessionCommand["action"] | undefined, result: CompanionResult): boolean {
  if (action === "local_session") {
    if (!result.ok) return isLocalSessionError(result.error);
    if ("installation_id" in result || "runtime_available" in result) return false;
    if (!localAction) return true;
    if (localAction === "state") return "version" in result && result.version === "2";
    if (localAction === "prepare") return "pending_id" in result;
    return !("version" in result) && !("pending_id" in result);
  }
  if (!result.ok) return isError(result.error);
  if (action === "sync") return !hasLocalState(result);
  if (action === "prepare") return hasLocalState(result) && "runtime_available" in result && typeof result.runtime_available === "boolean";
  return hasLocalState(result);
}

function parseResponse(value: unknown, action: CompanionAction): Readonly<{ id: number; result: CompanionResult }> | undefined {
  if (!isRecord(value) || !Number.isSafeInteger(value.id) || Number(value.id) < 1 || typeof value.ok !== "boolean") return undefined;
  if (value.ok) {
    if (action === "local_session") {
      if (!hasExactKeys(value, ["id", "ok", "local_session"])) return undefined;
      const result = parseLocalSessionResult(value.local_session);
      return result ? { id: Number(value.id), result } : undefined;
    }
    if (hasExactKeys(value, ["id", "ok"])) return { id: Number(value.id), result: { ok: true } };
    if (!hasExactKeys(value, ["id", "ok", "result"]) || !isRecord(value.result)) return undefined;
    const hasRelayActivity = Object.hasOwn(value.result, "relay_active");
    const fields = ["installation_id", "operating_system", "runtime_available", "cua_ready", "native_executor_ready", "gateway_connected", "relay_paused", "user_paused"];
    const expectedFields = hasRelayActivity ? [...fields, "relay_active"] : fields;
    if (hasExactKeys(value.result, expectedFields) &&
        isLocalState(value.result) && value.result.operating_system === "linux") {
      return { id: Number(value.id), result: { ok: true, ...value.result, operating_system: "linux" } };
    }
    return undefined;
  }
  if (!hasExactKeys(value, ["id", "ok", "error"])) return undefined;
  if (action === "local_session") {
    if (!isLocalSessionError(value.error)) return undefined;
    return { id: Number(value.id), result: { ok: false, error: value.error } };
  }
  if (!isError(value.error)) return undefined;
  return { id: Number(value.id), result: { ok: false, error: value.error } };
}

function parseLocalSessionResult(value: unknown): LocalSessionResult | undefined {
  if (!isRecord(value) || value.ok !== true) return undefined;
  if (hasExactKeys(value, ["ok"])) return { ok: true };
  if (hasExactKeys(value, ["ok", "version"]) && value.version === "2") return { ok: true, version: "2" };
  if (hasExactKeys(value, ["ok", "version", "harness"]) && value.version === "2" && isHarness(value.harness)) {
    return { ok: true, version: "2", harness: value.harness };
  }
  if (hasExactKeys(value, ["ok", "pending_id"]) && typeof value.pending_id === "string" &&
      /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value.pending_id)) {
    return { ok: true, pending_id: value.pending_id };
  }
  return undefined;
}

function isLocalState(value: Record<string, unknown>): value is Record<string, unknown> & DesktopControlState {
  return (typeof value.installation_id === "string" || value.installation_id === null) && value.operating_system === "linux" &&
    typeof value.runtime_available === "boolean" &&
    typeof value.cua_ready === "boolean" && typeof value.native_executor_ready === "boolean" &&
    typeof value.gateway_connected === "boolean" &&
    (!Object.hasOwn(value, "relay_active") || typeof value.relay_active === "boolean") &&
    typeof value.relay_paused === "boolean" && typeof value.user_paused === "boolean";
}

function hasLocalState(value: CompanionResult): value is Readonly<{ ok: true }> & (DesktopControlState | DesktopControlPrepared) {
  return value.ok && "installation_id" in value;
}

function isError(value: unknown): value is Exclude<DesktopControlError, "unsupported_platform" | "stale_request"> {
  return value === "invalid_request" || value === "not_enrolled" || value === "keyring_unavailable" ||
    value === "rejected" || value === "session_locked" || value === "session_state_unknown" || value === "unavailable";
}

function isLocalSessionError(value: unknown): value is LocalSessionError {
  return value === "invalid_request" || value === "invalid_bundle" || value === "stale_request" ||
    value === "missing_harness" || value === "outdated_harness" || value === "unsafe_files" || value === "unavailable";
}

function isHarness(value: unknown): value is "codex" | "claude_code" {
  return value === "codex" || value === "claude_code";
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function hasExactKeys(value: Record<string, unknown>, keys: readonly string[]): boolean {
  return Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key));
}
