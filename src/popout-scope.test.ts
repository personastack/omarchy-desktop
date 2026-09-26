import assert from "node:assert/strict";
import test from "node:test";

import { closePopoutWindows, synchronizePopoutScope } from "./popout-scope.js";

class WindowFake {
  closeCalls = 0;

  close(): void {
    this.closeCalls += 1;
  }
}

test("scope synchronization keeps same-scope popouts open", () => {
  const chat = new WindowFake();
  const activity = new WindowFake();
  const chats = new Map([["persona-1", chat]]);
  const activities = new Map([["persona-2", activity]]);

  const scope = synchronizePopoutScope("workspace-1", "workspace-1", [chats, activities], (window) => window.close());

  assert.equal(scope, "workspace-1");
  assert.equal(chat.closeCalls, 0);
  assert.equal(activity.closeCalls, 0);
  assert.equal(chats.size, 1);
  assert.equal(activities.size, 1);
});

test("scope changes close and clear every popout family", () => {
  const chat = new WindowFake();
  const graph = new WindowFake();
  const stream = new WindowFake();
  const activity = new WindowFake();
  const chats = new Map([["persona-1", chat]]);
  const stacks = new Map([["graph:stack-1", graph], ["stream:stack-1", stream]]);
  const activities = new Map([["activity:persona-2", activity]]);

  const scope = synchronizePopoutScope("workspace-1", "workspace-2", [chats, stacks, activities], (window) => window.close());

  assert.equal(scope, "workspace-2");
  assert.deepEqual([chat.closeCalls, graph.closeCalls, stream.closeCalls, activity.closeCalls], [1, 1, 1, 1]);
  assert.equal(chats.size + stacks.size + activities.size, 0);
});

test("hosted invalidation closes and clears all popout families", () => {
  const chat = new WindowFake();
  const stack = new WindowFake();
  const chats = new Map([["persona-1", chat]]);
  const stacks = new Map([["graph:stack-1", stack]]);

  closePopoutWindows([chats, stacks], (window) => window.close());

  assert.deepEqual([chat.closeCalls, stack.closeCalls], [1, 1]);
  assert.equal(chats.size + stacks.size, 0);
});
