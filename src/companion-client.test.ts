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
  await client.close();
  assert.equal(fake.isKilled(), false);
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
  await client.close();
});

test("companion client transports local-session commands and validates typed replies", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number; action: string; scope: string; local_session: object };
    assert.deepEqual(request, {
      id: 1,
      version: "1",
      action: "local_session",
      scope: "scope-a",
      local_session: { version: "1", action: "state", scope: "scope-a" },
    });
    fake.stdout.write(`${JSON.stringify({ id: request.id, ok: true, local_session: { ok: true, version: "2", harness: "codex" } })}\n`);
  });
  const result = await client.requestLocalSession({ version: "1", action: "state", scope: "scope-a" });
  assert.deepEqual(result, { ok: true, version: "2", harness: "codex" });
  await client.close();
});

test("companion client preserves finite local-session failure codes", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const pending = client.requestLocalSession({ version: "1", action: "prepare", scope: "scope-a", persona_id: "persona_1", harness: "codex" });
  fake.stdout.write('{"id":1,"ok":false,"error":"missing_harness"}\n');
  assert.deepEqual(await pending, { ok: false, error: "missing_harness" });
  await client.close();
});

test("companion client preserves finite session-lock failures", async () => {
  for (const error of ["session_locked", "session_state_unknown"] as const) {
    const fake = createFakeChild();
    const client = new CompanionClient(fake.child);
    const pending = client.request({ version: "1", action: "state", scope: "workspace:a" });
    fake.stdout.write(`${JSON.stringify({ id: 1, ok: false, error })}\n`);
    assert.deepEqual(await pending, { ok: false, error });
    await client.close();
  }
});

test("companion client rejects credential-bearing or malformed replies", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const resultPromise = client.request({ version: "1", action: "sync", scope: "" });
  fake.stdout.write('{"id":1,"ok":true,"machine_credential":"secret"}\n');
  assert.deepEqual(await resultPromise, { ok: false, error: "unavailable" });
  assert.equal(client.isOpen, false);
  await client.close();
  assert.equal(fake.isKilled(), false);
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
      result: { installation_id: "install_01", operating_system: "linux", runtime_available: true, cua_ready: true, native_executor_ready: false, gateway_connected: false, relay_paused: false },
    })}\n`);
  });
  const result = await client.request({ version: "1", action: "prepare", scope: "workspace:request", enrollment_ticket: "A".repeat(43) });
  assert.deepEqual(result, {
    ok: true,
    installation_id: "install_01",
    cua_ready: true,
    native_executor_ready: false,
    gateway_connected: false,
    relay_paused: false,
    operating_system: "linux",
    runtime_available: true,
  });
  await client.close();
});

test("prepare preserves a finite unavailable-runtime result", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const pending = client.request({ version: "1", action: "prepare", scope: "workspace:request", enrollment_ticket: "A".repeat(43) });
  fake.stdout.write('{"id":1,"ok":true,"result":{"installation_id":null,"operating_system":"linux","runtime_available":false,"cua_ready":false,"native_executor_ready":false,"gateway_connected":false,"relay_paused":true}}\n');
  assert.deepEqual(await pending, {
    ok: true,
    installation_id: null,
    operating_system: "linux",
    runtime_available: false,
    cua_ready: false,
    native_executor_ready: false,
    gateway_connected: false,
    relay_paused: true,
  });
  assert.equal(client.isOpen, true);
  await client.close();
});

test("companion client closes pending calls without replay", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const pending = client.request({ version: "1", action: "prepare", scope: "x", enrollment_ticket: "A".repeat(43) });
  const closing = client.close();
  assert.deepEqual(await pending, { ok: false, error: "unavailable" });
  await closing;
  assert.equal(client.isOpen, false);
});

test("companion client resolves graceful close when spawning fails", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const closing = client.close();
  fake.failSpawn();
  await closing;
  assert.equal(fake.isKilled(), false);
});

function createFakeChild(): Readonly<{
  child: ChildProcess;
  stdin: PassThrough;
  stdout: PassThrough;
  isKilled: () => boolean;
  failSpawn: () => void;
}> {
  const stdin = new PassThrough();
  const stdout = new PassThrough();
  const events = new EventEmitter();
  let killed = false;
  const child = Object.assign(events, {
    stdin,
    stdout,
    stderr: null,
    exitCode: null,
    signalCode: null,
    kill: () => {
      killed = true;
      events.emit("exit", null, "SIGTERM");
      return true;
    },
  }) as unknown as ChildProcess;
  stdin.on("finish", () => events.emit("exit", 0, null));
  return {
    child,
    stdin,
    stdout,
    isKilled: () => killed,
    failSpawn: () => {
      events.emit("error", new Error("spawn failed"));
      events.emit("close", -2, null);
    },
  };
}
