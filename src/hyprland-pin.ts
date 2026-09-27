import { execFile } from "node:child_process";
import { createConnection, type Socket } from "node:net";
import { join } from "node:path";

export interface HyprlandPinnedWindow {
  getNativeWindowHandle(): Buffer;
  isDestroyed(): boolean;
}

export interface HyprlandEventSocket {
  on(event: "data", listener: (data: Buffer) => void): this;
  on(event: "error", listener: (error: Error) => void): this;
  on(event: "close", listener: () => void): this;
  destroy(): void;
}

export interface HyprlandPinDependencies {
  readonly env: NodeJS.ProcessEnv;
  readonly run: (args: readonly string[]) => Promise<string>;
  readonly connect: (path: string) => HyprlandEventSocket;
}

interface HyprlandClient {
  readonly address: string;
  readonly xwaylandWindow: number;
  readonly workspace: number;
  readonly monitor: number;
}

interface HyprlandMonitor {
  readonly id: number;
  readonly activeWorkspace: number;
}

const refreshEvents = new Set([
  "activewindowv2",
  "focusedmonv2",
  "workspacev2",
  "movewindowv2",
  "openwindow",
  "closewindow",
]);

function runHyprctl(args: readonly string[]): Promise<string> {
  return new Promise((resolve, reject) => {
    execFile("hyprctl", [...args], { encoding: "utf8", timeout: 1500, maxBuffer: 4 << 20 }, (error, stdout) => {
      if (error) {
        reject(error);
        return;
      }
      resolve(stdout);
    });
  });
}

function connectHyprlandEvents(path: string): Socket {
  return createConnection(path);
}

function record(value: unknown): Record<string, unknown> | undefined {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : undefined;
}

function positiveInteger(value: unknown): number | undefined {
  if (typeof value === "number" && Number.isSafeInteger(value) && value > 0) return value;
  if (typeof value !== "string" || !/^(?:0x[\da-f]+|\d+)$/i.test(value)) return undefined;
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : undefined;
}

function safeInteger(value: unknown): number | undefined {
  return typeof value === "number" && Number.isSafeInteger(value) ? value : undefined;
}

function parseClients(raw: string): HyprlandClient[] {
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return [];
  }
  if (!Array.isArray(value)) return [];
  return value.flatMap((candidate) => {
    const item = record(candidate);
    const workspace = record(item?.workspace);
    const address = typeof item?.address === "string" && /^0x[\da-f]+$/i.test(item.address)
      ? item.address
      : undefined;
    const xwaylandWindow = item?.xwayland === true ? positiveInteger(item.xwaylandWindow) : undefined;
    const workspaceID = safeInteger(workspace?.id);
    const monitor = safeInteger(item?.monitor);
    return address && xwaylandWindow !== undefined && workspaceID !== undefined && monitor !== undefined
      ? [{ address, xwaylandWindow, workspace: workspaceID, monitor }]
      : [];
  });
}

function parseMonitors(raw: string): HyprlandMonitor[] {
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return [];
  }
  if (!Array.isArray(value)) return [];
  return value.flatMap((candidate) => {
    const item = record(candidate);
    const activeWorkspace = record(item?.activeWorkspace);
    const id = safeInteger(item?.id);
    const workspace = safeInteger(activeWorkspace?.id);
    return id !== undefined && workspace !== undefined ? [{ id, activeWorkspace: workspace }] : [];
  });
}

function x11WindowID(window: HyprlandPinnedWindow): number | undefined {
  if (window.isDestroyed()) return undefined;
  try {
    const handle = window.getNativeWindowHandle();
    if (handle.length < 4) return undefined;
    return positiveInteger(handle.readUInt32LE(0));
  } catch {
    return undefined;
  }
}

export class HyprlandPinAdapter {
  private readonly pinned = new Map<number, HyprlandPinnedWindow>();
  private readonly socketPath: string | undefined;
  private socket: HyprlandEventSocket | undefined;
  private eventBuffer = "";
  private refreshTimer: NodeJS.Timeout | undefined;
  private retryTimer: NodeJS.Timeout | undefined;
  private refreshing = false;
  private refreshPending = false;

  constructor(private readonly dependencies: HyprlandPinDependencies = {
    env: process.env,
    run: runHyprctl,
    connect: connectHyprlandEvents,
  }) {
    const runtimeDirectory = dependencies.env.XDG_RUNTIME_DIR;
    const signature = dependencies.env.HYPRLAND_INSTANCE_SIGNATURE;
    if (runtimeDirectory?.startsWith("/") && signature && /^[\da-z_-]+$/i.test(signature)) {
      this.socketPath = join(runtimeDirectory, "hypr", signature, ".socket2.sock");
    }
  }

  async setPinned(window: HyprlandPinnedWindow, pinned: boolean): Promise<boolean> {
    if (!pinned) {
      for (const [id, pinnedWindow] of this.pinned) {
        if (pinnedWindow === window) this.pinned.delete(id);
      }
      if (this.pinned.size === 0) this.stopEvents();
      return true;
    }
    const id = x11WindowID(window);
    if (!this.socketPath || id === undefined) return false;
    this.pinned.set(id, window);
    this.connectEvents();
    await this.refresh();
    return true;
  }

  close(): void {
    this.pinned.clear();
    this.stopEvents();
  }

  private connectEvents(): void {
    if (!this.socketPath || this.socket || this.pinned.size === 0) return;
    let socket: HyprlandEventSocket;
    try {
      socket = this.dependencies.connect(this.socketPath);
    } catch {
      this.scheduleReconnect();
      return;
    }
    this.socket = socket;
    socket.on("data", (data) => this.handleData(data));
    socket.on("error", () => this.handleSocketClose(socket));
    socket.on("close", () => this.handleSocketClose(socket));
  }

  private handleData(data: Buffer): void {
    this.eventBuffer += data.toString("utf8");
    if (this.eventBuffer.length > 16_384) this.eventBuffer = "";
    const lines = this.eventBuffer.split("\n");
    this.eventBuffer = lines.pop() ?? "";
    for (const line of lines) {
      const separator = line.indexOf(">>");
      if (separator > 0 && refreshEvents.has(line.slice(0, separator))) this.scheduleRefresh();
    }
  }

  private handleSocketClose(socket: HyprlandEventSocket): void {
    if (this.socket !== socket) return;
    this.socket = undefined;
    socket.destroy();
    if (this.pinned.size > 0) this.scheduleReconnect();
  }

  private scheduleReconnect(): void {
    if (this.retryTimer || this.pinned.size === 0) return;
    this.retryTimer = setTimeout(() => {
      this.retryTimer = undefined;
      this.connectEvents();
      if (this.socket) this.scheduleRefresh();
    }, 1000);
    this.retryTimer.unref();
  }

  private scheduleRefresh(): void {
    if (this.pinned.size === 0) return;
    if (this.refreshTimer) clearTimeout(this.refreshTimer);
    this.refreshTimer = setTimeout(() => {
      this.refreshTimer = undefined;
      void this.refresh();
    }, 40);
    this.refreshTimer.unref();
  }

  private async refresh(): Promise<void> {
    if (this.refreshing) {
      this.refreshPending = true;
      return;
    }
    this.refreshing = true;
    try {
      const [clientJSON, monitorJSON] = await Promise.all([
        this.dependencies.run(["-j", "clients"]),
        this.dependencies.run(["-j", "monitors"]),
      ]);
      const clients = parseClients(clientJSON);
      const monitors = parseMonitors(monitorJSON);
      for (const [id, window] of this.pinned) {
        if (window.isDestroyed()) {
          this.pinned.delete(id);
          continue;
        }
        const client = clients.find((candidate) => candidate.xwaylandWindow === id);
        const visibleWorkspace = client && monitors.some((monitor) =>
          monitor.id === client.monitor && monitor.activeWorkspace === client.workspace,
        );
        if (client && visibleWorkspace) {
          await this.dependencies.run(["dispatch", "alterzorder", `top,address:${client.address}`]);
        }
      }
    } catch {
      // The Electron always-on-top request remains in place when the Hyprland adapter is unavailable.
    } finally {
      this.refreshing = false;
      if (this.refreshPending) {
        this.refreshPending = false;
        this.scheduleRefresh();
      }
      if (this.pinned.size === 0) this.stopEvents();
    }
  }

  private stopEvents(): void {
    if (this.refreshTimer) clearTimeout(this.refreshTimer);
    if (this.retryTimer) clearTimeout(this.retryTimer);
    this.refreshTimer = undefined;
    this.retryTimer = undefined;
    this.refreshPending = false;
    const socket = this.socket;
    this.socket = undefined;
    this.eventBuffer = "";
    socket?.destroy();
  }
}
