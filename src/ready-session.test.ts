import assert from "node:assert/strict";
import test from "node:test";

import { createAfterReady } from "./ready-session.js";

test("persistent Electron session creation waits for app readiness", async () => {
  let markReady: (() => void) | undefined;
  let ready = false;
  const readiness = new Promise<void>((resolve) => { markReady = () => { ready = true; resolve(); }; });
  let createCount = 0;
  const result = createAfterReady(readiness, () => {
    assert.equal(ready, true);
    createCount++;
    return "session";
  });

  assert.equal(createCount, 0);
  markReady?.();
  assert.equal(await result, "session");
  assert.equal(createCount, 1);
});
