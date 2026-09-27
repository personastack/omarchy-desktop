import assert from "node:assert/strict";
import test from "node:test";

import {
  applyChatWindowCommand,
  type ChatWindowCommandDependencies,
  type ChatWindowCommandTarget,
} from "./chat-window-command.js";
import type { ChatWindowCommand } from "./security.js";

class FakeChatWindow implements ChatWindowCommandTarget {
  destroyed = false;
  resizable = true;
  position: [number, number] = [100, 80];
  size: [number, number] = [440, 640];
  minimumSize: [number, number] = [340, 360];
  alwaysOnTop = false;
  alwaysOnTopLevel: string | undefined;
  minimized = false;

  isDestroyed(): boolean { return this.destroyed; }
  getSize(): number[] { return this.size; }
  setSize(width: number, height: number): void {
    this.size = [Math.max(width, this.minimumSize[0]), Math.max(height, this.minimumSize[1])];
  }
  setMinimumSize(width: number, height: number): void { this.minimumSize = [width, height]; }
  setResizable(resizable: boolean): void { this.resizable = resizable; }
  minimize(): void { this.minimized = true; }
  isAlwaysOnTop(): boolean { return this.alwaysOnTop; }
  setAlwaysOnTop(flag: boolean, level?: string): void {
    this.alwaysOnTop = flag;
    this.alwaysOnTopLevel = level;
  }
  getPosition(): number[] { return this.position; }
  setPosition(x: number, y: number): void { this.position = [x, y]; }
}

function command(action: ChatWindowCommand["action"]): ChatWindowCommand {
  return action === "drag"
    ? { version: "1", action, dx: 0, dy: 0 }
    : { version: "1", action };
}

function dependencies(): ChatWindowCommandDependencies & { readonly closed: number; readonly matchedBounds: unknown[] } {
  const state = { closed: 0, matchedBounds: [] as unknown[] };
  return {
    get closed() { return state.closed; },
    matchedBounds: state.matchedBounds,
    close: () => { state.closed += 1; },
    workAreaFor: (bounds) => {
      state.matchedBounds.push(bounds);
      return { x: 0, y: 0, width: 1000, height: 800 };
    },
  };
}

test("chat window commands map minimize and close to their native owners", () => {
  const window = new FakeChatWindow();
  const deps = dependencies();
  applyChatWindowCommand(command("minimize"), window, undefined, deps);
  applyChatWindowCommand(command("close"), window, undefined, deps);

  assert.equal(window.minimized, true);
  assert.equal(deps.closed, 1);
});

test("chat window commands preserve collapse size and expand state", () => {
  const window = new FakeChatWindow();
  const deps = dependencies();
  const saved = applyChatWindowCommand(command("collapse"), window, undefined, deps);
  assert.equal(window.resizable, false);
  assert.deepEqual(window.size, [72, 72]);
  assert.deepEqual(saved, [440, 640]);

  applyChatWindowCommand(command("expand"), window, saved, deps);
  assert.equal(window.resizable, true);
  assert.deepEqual(window.minimumSize, [340, 360]);
  assert.deepEqual(window.size, [440, 640]);
});

test("pin toggles native floating placement", () => {
  const window = new FakeChatWindow();
  const deps = dependencies();
  applyChatWindowCommand(command("pin"), window, undefined, deps);
  assert.equal(window.alwaysOnTop, true);
  assert.equal(window.alwaysOnTopLevel, "floating");
  applyChatWindowCommand(command("pin"), window, undefined, deps);
  assert.equal(window.alwaysOnTop, false);
});

test("drag selects the destination display and clamps rounded native coordinates", () => {
  const window = new FakeChatWindow();
  const deps = dependencies();
  applyChatWindowCommand({ version: "1", action: "drag", dx: 900.5, dy: 700.5 }, window, undefined, deps);

  assert.deepEqual(deps.matchedBounds, [{ x: 1000.5, y: 780.5, width: 440, height: 640 }]);
  assert.deepEqual(window.position, [560, 160]);
});
