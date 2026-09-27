import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const PNG_SIGNATURE = Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]);

test("build copies a non-empty PNG native icon into the app runtime", () => {
  const image = readFileSync(new URL("./assets/personastack.png", import.meta.url));
  assert.deepEqual(image.subarray(0, PNG_SIGNATURE.length), PNG_SIGNATURE);
  assert.equal(image.toString("ascii", 12, 16), "IHDR");
  assert.ok(image.readUInt32BE(16) > 0);
  assert.ok(image.readUInt32BE(20) > 0);
});
