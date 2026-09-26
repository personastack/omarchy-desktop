import type { ChildProcess } from "node:child_process";

import type { DesktopControlCommand } from "./security.js";

const MAX_RESPONSE_BYTES = 4 * 1024;
const MAX_PENDING_REQUESTS = 8;
const REQUEST_TIMEOUT_MS = 45_000;
const MAX_REQUEST_ID = Number.MAX_SAFE_INTEGER;

export type DesktopControlState = Readonly<{
  installation_id: string | null;
  operating_system: "linux";
  cua_ready: boolean;
  native_executor_ready: boolean;
  gateway_connected: boolean;
  relay_paused: boolean;
}>;

type DesktopControlPrepared = DesktopControlState & Readonly<{ runtime_available: false }>;

export type DesktopControlError =
  | "invalid_request"
  | "not_enrolled"
  | "keyring_unavailable"
  | "rejected"
  | "unavailable"
  | "unsupported_platform"
  | "stale_request";

export type DesktopControlResult =
  | Readonly<{ ok: true }>
  | (Readonly<{ ok: true }> & DesktopControlState)
  | (Readonly<{ ok: true }> & DesktopControlPrepared)
  | Readonly<{ ok: false; error: DesktopControlError }>;

type PendingRequest = Readonly<{
  action: DesktopControlCommand["action"];
  resolve: (result: DesktopControlResult) => void;
  timeout: NodeJS.Timeout;
}>;

export class CompanionClient {
  private readonly pending = new Map<number, PendingRequest>();
  private output = "";
  private nextRequestID = 1;
  private closed = false;
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
    if (this.closed || this.pending.size >= MAX_PENDING_REQUESTS || this.nextRequestID > MAX_REQUEST_ID) {
      return Promise.resolve({ ok: false, error: "unavailable" });
    }

    const id = this.nextRequestID++;
    const request = { id, ...command };
    return new Promise((resolve) => {
      const timeout = setTimeout(() => this.failAll(), REQUEST_TIMEOUT_MS);
      this.pending.set(id, { action: command.action, resolve, timeout });
      try {
        this.child.stdin?.write(`${JSON.stringify(request)}\n`, (error) => {
          if (error) this.failAll();
        });
      } catch {
        this.failAll();
      }
    });
  }

  close(): void {
    if (this.closed) return;
    this.failAll();
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
    const response = parseResponse(value);
    if (!response) return false;
    const pending = this.pending.get(response.id);
    if (!pending || !matchesCommandResponse(pending.action, response.result)) return false;
    clearTimeout(pending.timeout);
    this.pending.delete(response.id);
    pending.resolve(response.result);
    return true;
  }

  private failAll(): void {
    if (this.closed) return;
    this.closed = true;
    for (const pending of this.pending.values()) {
      clearTimeout(pending.timeout);
      pending.resolve({ ok: false, error: "unavailable" });
    }
    this.pending.clear();
    this.child.kill("SIGTERM");
  }
}

function matchesCommandResponse(action: DesktopControlCommand["action"], result: DesktopControlResult): boolean {
  if (!result.ok) return true;
  if (action === "sync") return !hasLocalState(result);
  if (action === "prepare") return hasLocalState(result) && "runtime_available" in result && result.runtime_available === false;
  if ("runtime_available" in result) return false;
  return hasLocalState(result);
}

function parseResponse(value: unknown): Readonly<{ id: number; result: DesktopControlResult }> | undefined {
  if (!isRecord(value) || !Number.isSafeInteger(value.id) || Number(value.id) < 1 || typeof value.ok !== "boolean") return undefined;
  if (value.ok) {
    if (hasExactKeys(value, ["id", "ok"])) return { id: Number(value.id), result: { ok: true } };
    if (!hasExactKeys(value, ["id", "ok", "result"]) || !isRecord(value.result)) return undefined;
    if (hasExactKeys(value.result, ["installation_id", "operating_system", "cua_ready", "native_executor_ready", "gateway_connected", "relay_paused"]) &&
        isLocalState(value.result) && value.result.operating_system === "linux") {
      return { id: Number(value.id), result: { ok: true, ...value.result, operating_system: "linux" } };
    }
    if (hasExactKeys(value.result, ["installation_id", "operating_system", "runtime_available", "cua_ready", "native_executor_ready", "gateway_connected", "relay_paused"]) &&
        isLocalState(value.result) && value.result.operating_system === "linux" && value.result.runtime_available === false) {
      return { id: Number(value.id), result: { ok: true, ...value.result, operating_system: "linux", runtime_available: false } };
    }
    return undefined;
  }
  if (!hasExactKeys(value, ["id", "ok", "error"]) || !isError(value.error)) return undefined;
  return { id: Number(value.id), result: { ok: false, error: value.error } };
}

function isLocalState(value: Record<string, unknown>): value is Record<string, unknown> & DesktopControlState {
  return (typeof value.installation_id === "string" || value.installation_id === null) && value.operating_system === "linux" &&
    typeof value.cua_ready === "boolean" && typeof value.native_executor_ready === "boolean" &&
    typeof value.gateway_connected === "boolean" && typeof value.relay_paused === "boolean";
}

function hasLocalState(value: DesktopControlResult): value is Readonly<{ ok: true }> & (DesktopControlState | DesktopControlPrepared) {
  return value.ok && "installation_id" in value;
}

function isError(value: unknown): value is Exclude<DesktopControlError, "unsupported_platform" | "stale_request"> {
  return value === "invalid_request" || value === "not_enrolled" || value === "keyring_unavailable" ||
    value === "rejected" || value === "unavailable";
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function hasExactKeys(value: Record<string, unknown>, keys: readonly string[]): boolean {
  return Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key));
}
