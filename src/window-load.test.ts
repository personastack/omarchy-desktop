import assert from "node:assert/strict";
import test from "node:test";

import { loadAndShowWindow } from "./window-load.js";

test("loaded popout only shows while it remains current", async () => {
  let current = true;
  let showCalls = 0;
  let closeCalls = 0;

  await loadAndShowWindow({
    load: async () => undefined,
    isCurrent: () => current,
    show: () => { showCalls += 1; },
    close: () => { closeCalls += 1; },
  });

  assert.equal(showCalls, 1);
  assert.equal(closeCalls, 0);

  let finishLoad!: () => void;
  const pendingLoad = new Promise<void>((resolve) => { finishLoad = resolve; });
  const loading = loadAndShowWindow({
    load: () => pendingLoad,
    isCurrent: () => current,
    show: () => { showCalls += 1; },
    close: () => { closeCalls += 1; },
  });
  current = false;
  finishLoad();
  await loading;

  assert.equal(showCalls, 1);
  assert.equal(closeCalls, 0);
});

test("failed popout load is handled by closing the window", async () => {
  let showCalls = 0;
  let closeCalls = 0;

  await loadAndShowWindow({
    load: async () => { throw new Error("navigation failed"); },
    isCurrent: () => true,
    show: () => { showCalls += 1; },
    close: () => { closeCalls += 1; },
  });

  assert.equal(showCalls, 0);
  assert.equal(closeCalls, 1);
});
