import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import type { ChildProcess } from "node:child_process";
import { PassThrough } from "node:stream";
import test from "node:test";

import { CompanionClient } from "./companion-client.js";

test("companion client correlates local state replies", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number; action: string; version: string; scope: string };
    assert.deepEqual(request, { id: 1, version: "1", action: "state", scope: "" });
    fake.stdout.write(`${JSON.stringify({
      id: request.id,
      ok: true,
      result: { installation_id: null, operating_system: "linux", cua_ready: false, native_executor_ready: false, gateway_connected: false, relay_paused: true },
    })}\n`);
  });
  const result = await client.request({ version: "1", action: "state", scope: "" });
  assert.deepEqual(result, {
    ok: true,
    installation_id: null,
    operating_system: "linux",
    cua_ready: false,
    native_executor_ready: false,
    gateway_connected: false,
    relay_paused: true,
  });
  assert.equal(client.isOpen, true);
  client.close();
  assert.equal(fake.isKilled(), true);
});

test("companion client accepts the sync acknowledgment shape", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number; version: string; action: string; scope: string };
    assert.deepEqual(request, { id: 1, version: "1", action: "sync", scope: "workspace:request" });
    fake.stdout.write(`${JSON.stringify({ id: request.id, ok: true })}\n`);
  });
  assert.deepEqual(await client.request({ version: "1", action: "sync", scope: "workspace:request" }), { ok: true });
  client.close();
});

test("companion client rejects credential-bearing or malformed replies", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const resultPromise = client.request({ version: "1", action: "sync", scope: "" });
  fake.stdout.write('{"id":1,"ok":true,"machine_credential":"secret"}\n');
  assert.deepEqual(await resultPromise, { ok: false, error: "unavailable" });
  assert.equal(client.isOpen, false);
  assert.equal(fake.isKilled(), true);
});

test("companion client requires response shape to match its request", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const pending = client.request({ version: "1", action: "state", scope: "" });
  fake.stdout.write('{"id":1,"ok":true}\n');
  assert.deepEqual(await pending, { ok: false, error: "unavailable" });
  assert.equal(client.isOpen, false);
});

test("prepare response includes a Linux platform and local readiness", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number };
    fake.stdout.write(`${JSON.stringify({
      id: request.id,
      ok: true,
      result: { installation_id: "install_01", operating_system: "linux", runtime_available: false, cua_ready: false, native_executor_ready: false, gateway_connected: false, relay_paused: true },
    })}\n`);
  });
  const result = await client.request({ version: "1", action: "prepare", scope: "workspace:request", enrollment_ticket: "A".repeat(43) });
  assert.deepEqual(result, {
    ok: true,
    installation_id: "install_01",
    cua_ready: false,
    native_executor_ready: false,
    gateway_connected: false,
    relay_paused: true,
    operating_system: "linux",
    runtime_available: false,
  });
  client.close();
});

test("companion client closes pending calls without replay", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const pending = client.request({ version: "1", action: "prepare", scope: "x", enrollment_ticket: "A".repeat(43) });
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
