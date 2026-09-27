import assert from "node:assert/strict";
import test from "node:test";

import {
  beginNavigationGeneration,
  commitNavigationGeneration,
  rollbackNavigationGeneration,
} from "./navigation-generation.js";

test("a canceled navigation restores the generation used by the current document", () => {
  const started = beginNavigationGeneration({ current: 4 });

  assert.deepEqual(started, { current: 5, beforeNavigation: 4 });
  assert.deepEqual(rollbackNavigationGeneration(started), { current: 4 });
});

test("a committed navigation keeps its new preload generation", () => {
  const started = beginNavigationGeneration({ current: 4 });

  assert.deepEqual(commitNavigationGeneration(started), { current: 5 });
});

test("redirect starts retain the original generation for cancellation", () => {
  const firstStart = beginNavigationGeneration({ current: 4 });
  const redirectStart = beginNavigationGeneration(firstStart);

  assert.deepEqual(redirectStart, { current: 6, beforeNavigation: 4 });
  assert.deepEqual(rollbackNavigationGeneration(redirectStart), { current: 4 });
});

test("a navigation failure restores the existing document generation", () => {
  const started = beginNavigationGeneration({ current: 4 });

  assert.deepEqual(rollbackNavigationGeneration(started), { current: 4 });
});
