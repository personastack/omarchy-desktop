import assert from "node:assert/strict";
import test from "node:test";

import {
  applyChatWindowSizeAction,
  clampChatPosition,
  collapseChatSize,
  expandedChatSize,
  type ChatWindowSizer,
} from "./chat-window-state.js";

class FakeChatWindow implements ChatWindowSizer {
  destroyed = false;
  resizable = true;
  position: [number, number] = [100, 80];
  size: [number, number] = [440, 640];
  minimumSize: [number, number] = [340, 360];

  isDestroyed(): boolean {
    return this.destroyed;
  }

  getSize(): number[] {
    return this.size;
  }

  getPosition(): number[] {
    return this.position;
  }

  setSize(width: number, height: number): void {
    this.size = [Math.max(width, this.minimumSize[0]), Math.max(height, this.minimumSize[1])];
  }

  setPosition(x: number, y: number): void {
    this.position = [x, y];
  }

  setMinimumSize(width: number, height: number): void {
    this.minimumSize = [width, height];
  }

  setResizable(resizable: boolean): void {
    this.resizable = resizable;
  }
}

const workArea = { x: 0, y: 0, width: 1000, height: 800 };
const workAreaFor = () => workArea;

test("collapse remembers expanded dimensions and expansion restores them", () => {
  const collapsed = collapseChatSize([520, 720]);

  assert.deepEqual(collapsed, { size: [72, 72], restoreSize: [520, 720] });
  assert.deepEqual(expandedChatSize(collapsed.restoreSize), [520, 720]);
});

test("repeated collapse preserves the previously saved expanded dimensions", () => {
  const collapsed = collapseChatSize([72, 72], [520, 720]);

  assert.deepEqual(collapsed, { size: [72, 72], restoreSize: [520, 720] });
  assert.deepEqual(expandedChatSize(collapsed.restoreSize), [520, 720]);
});

test("collapse lowers the minimum before resizing and restores the expanded size", () => {
  const window = new FakeChatWindow();
  const saved = applyChatWindowSizeAction("collapse", window, undefined, workAreaFor);

  assert.equal(window.resizable, false);
  assert.deepEqual(window.minimumSize, [72, 72]);
  assert.deepEqual(window.size, [72, 72]);
  assert.deepEqual(saved, [440, 640]);

  const savedAgain = applyChatWindowSizeAction("collapse", window, saved, workAreaFor);
  assert.deepEqual(savedAgain, [440, 640]);

  const restored = applyChatWindowSizeAction("expand", window, savedAgain, workAreaFor);
  assert.deepEqual(window.minimumSize, [340, 360]);
  assert.equal(window.resizable, true);
  assert.deepEqual(window.size, [440, 640]);
  assert.deepEqual(restored, [440, 640]);
});

test("destroyed chat windows do not receive size operations", () => {
  const window = new FakeChatWindow();
  window.destroyed = true;

  assert.deepEqual(applyChatWindowSizeAction("collapse", window, [520, 720], workAreaFor), [520, 720]);
  assert.deepEqual(window.size, [440, 640]);
  assert.deepEqual(window.minimumSize, [340, 360]);
});

test("expand clamps the restored size into the current display work area", () => {
  const window = new FakeChatWindow();
  window.position = [900, 700];
  const saved = applyChatWindowSizeAction("collapse", window, undefined, workAreaFor);
  assert.deepEqual(window.position, [900, 700]);

  applyChatWindowSizeAction("expand", window, saved, workAreaFor);
  assert.deepEqual(window.position, [560, 160]);
});

test("repeated expand preserves a user's current expanded size", () => {
  const window = new FakeChatWindow();
  const saved = applyChatWindowSizeAction("collapse", window, undefined, workAreaFor);
  applyChatWindowSizeAction("expand", window, saved, workAreaFor);
  window.size = [500, 600];

  const repeated = applyChatWindowSizeAction("expand", window, saved, workAreaFor);
  assert.deepEqual(window.size, [500, 600]);
});

test("expansion uses the standard size when no saved size exists", () => {
  assert.deepEqual(expandedChatSize(), [440, 640]);
});

test("drag clamps each edge to the matching display work area", () => {
  assert.deepEqual(
    clampChatPosition([900, 700], [440, 640], { x: 100, y: 80, width: 800, height: 700 }),
    [460, 140],
  );
  assert.deepEqual(
    clampChatPosition([-200, -100], [440, 640], { x: 100, y: 80, width: 800, height: 700 }),
    [100, 80],
  );
});

test("fractional native drag coordinates are rounded before position clamping", () => {
  assert.deepEqual(
    clampChatPosition([100.5, 80.5], [440, 640], { x: 100, y: 80, width: 800, height: 700 }),
    [101, 81],
  );
});

test("drag keeps an oversized window anchored inside the work area", () => {
  assert.deepEqual(
    clampChatPosition([900, 700], [1200, 900], { x: 100, y: 80, width: 800, height: 700 }),
    [100, 80],
  );
});
