import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import test from "node:test";

import { HyprlandPinAdapter, type HyprlandEventSocket, type HyprlandPinnedWindow } from "./hyprland-pin.js";

class FakeSocket implements HyprlandEventSocket {
  private readonly events = new EventEmitter();

  on(event: "data", listener: (data: Buffer) => void): this;
  on(event: "error", listener: (error: Error) => void): this;
  on(event: "close", listener: () => void): this;
  on(event: "data" | "error" | "close", listener: ((data: Buffer) => void) | ((error: Error) => void) | (() => void)): this {
    this.events.on(event, listener as unknown as (...args: unknown[]) => void);
    return this;
  }

  destroy(): void {
    this.events.emit("close");
  }

  send(data: string): void {
    this.events.emit("data", Buffer.from(data));
  }
}

class FakeWindow implements HyprlandPinnedWindow {
  readonly handle = Buffer.alloc(4);

  constructor(id: number) {
    this.handle.writeUInt32LE(id);
  }

  getNativeWindowHandle(): Buffer {
    return this.handle;
  }

  isDestroyed(): boolean {
    return false;
  }
}

function profile(workspace: number, monitor: number): { readonly clients: string; readonly monitors: string } {
  return {
    clients: JSON.stringify([{
      address: "0xabc123",
      xwayland: true,
      xwaylandWindow: 4_194_305,
      workspace: { id: workspace },
      monitor,
    }]),
    monitors: JSON.stringify([
      { id: 0, activeWorkspace: { id: 1 } },
      { id: 1, activeWorkspace: { id: 2 } },
    ]),
  };
}

async function waitForCalls(calls: readonly string[][], count: number): Promise<void> {
  const deadline = Date.now() + 1000;
  while (calls.length < count && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}

test("pinned chat rises on its active workspace without invoking focus", async () => {
  const calls: string[][] = [];
  const socket = new FakeSocket();
  const current = profile(2, 1);
  const adapter = new HyprlandPinAdapter({
    env: { XDG_RUNTIME_DIR: "/run/user/1000", HYPRLAND_INSTANCE_SIGNATURE: "test-1" },
    run: async (args) => {
      calls.push([...args]);
      if (args[0] === "-j" && args[1] === "clients") return current.clients;
      if (args[0] === "-j" && args[1] === "monitors") return current.monitors;
      return "ok";
    },
    connect: () => socket,
  });

  assert.equal(await adapter.setPinned(new FakeWindow(4_194_305), true), true);
  const initialRaise = ["dispatch", "alterzorder", "top,address:0xabc123"];
  assert.equal(calls.some((args) => JSON.stringify(args) === JSON.stringify(initialRaise)), true);

  const previousCount = calls.length;
  socket.send("activewindowv2>>0xdef456\n");
  await waitForCalls(calls, previousCount + 3);
  assert.equal(calls.some((args) => JSON.stringify(args) === JSON.stringify(initialRaise)), true);
  assert.equal(calls.some((args) => args[0] === "dispatch" && args[1] === "focus"), false);
  adapter.close();
});

test("pinned chat is not raised when its workspace is inactive on its monitor", async () => {
  const calls: string[][] = [];
  const socket = new FakeSocket();
  const inactive = profile(3, 1);
  const adapter = new HyprlandPinAdapter({
    env: { XDG_RUNTIME_DIR: "/run/user/1000", HYPRLAND_INSTANCE_SIGNATURE: "test-1" },
    run: async (args) => {
      calls.push([...args]);
      if (args[0] === "-j" && args[1] === "clients") return inactive.clients;
      if (args[0] === "-j" && args[1] === "monitors") return inactive.monitors;
      return "ok";
    },
    connect: () => socket,
  });

  assert.equal(await adapter.setPinned(new FakeWindow(4_194_305), true), true);
  assert.equal(calls.some((args) => args[0] === "dispatch"), false);
  adapter.close();
});

test("pin adapter rejects unsafe Hyprland instance signatures", async () => {
  const target = new FakeWindow(4_194_305);
  const adapter = new HyprlandPinAdapter({
    env: { XDG_RUNTIME_DIR: "/run/user/1000", HYPRLAND_INSTANCE_SIGNATURE: "../other" },
    run: async () => "[]",
    connect: () => new FakeSocket(),
  });

  assert.equal(await adapter.setPinned(target, true), false);
});
