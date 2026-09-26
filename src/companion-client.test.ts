import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import type { ChildProcess } from "node:child_process";
import { PassThrough } from "node:stream";
import test from "node:test";

import { CompanionClient } from "./companion-client.js";

test("companion client correlates typed requests and status responses", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number; action: string };
    assert.deepEqual(request, { id: 1, action: "status" });
    fake.stdout.write(`${JSON.stringify({
      id: request.id,
      ok: true,
      status: { enrolled: true, credential_valid: true, relay_active: false },
    })}\n`);
  });
  const result = await client.request({ version: "1", action: "status" });
  assert.deepEqual(result, {
    ok: true,
    status: { enrolled: true, credential_valid: true, relay_active: false },
  });
  assert.equal(client.isOpen, true);
  client.close();
  assert.equal(fake.isKilled(), true);
});

test("companion client rejects credential-bearing or malformed replies", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const resultPromise = client.request({ version: "1", action: "status" });
  fake.stdout.write('{"id":1,"ok":true,"status":{"enrolled":true,"credential_valid":true,"relay_active":false},"machine_credential":"secret"}\n');
  assert.deepEqual(await resultPromise, { ok: false, error: "unavailable" });
  assert.equal(client.isOpen, false);
  assert.equal(fake.isKilled(), true);
});

test("companion client requires status replies to match their request", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const pending = client.request({ version: "1", action: "status" });
  fake.stdout.write('{"id":1,"ok":true}\n');
  assert.deepEqual(await pending, { ok: false, error: "unavailable" });
  assert.equal(client.isOpen, false);
});

test("companion client closes pending calls without replay", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const pending = client.request({ version: "1", action: "enroll", ticket: "A".repeat(43) });
  client.close();
  assert.deepEqual(await pending, { ok: false, error: "unavailable" });
  assert.equal(client.isOpen, false);
});

function createFakeChild(): Readonly<{
  child: ChildProcess;
  stdin: PassThrough;
  stdout: PassThrough;
  isKilled: () => boolean;
}> {
  const stdin = new PassThrough();
  const stdout = new PassThrough();
  const events = new EventEmitter();
  let killed = false;
  const child = Object.assign(events, {
    stdin,
    stdout,
    stderr: null,
    kill: () => {
      killed = true;
      return true;
    },
  }) as unknown as ChildProcess;
  return { child, stdin, stdout, isKilled: () => killed };
}
