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
    if (request.action === "sync") {
      assert.deepEqual(request, { id: 1, version: "1", action: "sync", scope: "" });
      fake.stdout.write(`${JSON.stringify({ id: request.id, ok: true })}\n`);
      return;
    }
    assert.deepEqual(request, { id: 2, version: "1", action: "state", scope: "" });
    fake.stdout.write(`${JSON.stringify({
      id: request.id,
      ok: true,
      result: { installation_id: null, operating_system: "linux", runtime_available: false, cua_ready: false, cua_upgrade_required: false, native_executor_ready: false, gateway_connected: false, relay_paused: true, user_paused: false },
    })}\n`);
  });
  const result = await client.request({ version: "1", action: "state", scope: "" });
  assert.deepEqual(result, {
    ok: true,
    installation_id: null,
    operating_system: "linux",
    runtime_available: false,
    cua_ready: false,
    cua_upgrade_required: false,
    native_executor_ready: false,
    gateway_connected: false,
    relay_paused: true,
    user_paused: false,
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

test("companion client accepts typed tray lifecycle state replies", async () => {
  for (const action of ["pause", "resume", "repair", "disconnect"] as const) {
    const fake = createFakeChild();
    const client = new CompanionClient(fake.child);
    fake.stdin.on("data", (chunk: Buffer) => {
      const request = JSON.parse(chunk.toString("utf8")) as { id: number; version: string; action: string; scope: string };
      assert.equal(request.action, action);
      assert.equal(request.scope, "desktop:lifecycle");
      fake.stdout.write(`${JSON.stringify({
        id: request.id,
        ok: true,
        result: { installation_id: null, operating_system: "linux", runtime_available: true, cua_ready: false, cua_upgrade_required: false, native_executor_ready: false, gateway_connected: false, relay_active: true, relay_paused: action === "pause", user_paused: action === "pause" },
      })}\n`);
    });
    const result = await client.requestLifecycle(action);
    assert.equal(result.ok, true);
    if (result.ok && "relay_paused" in result) assert.equal(result.relay_paused, action === "pause");
    else assert.fail(`missing lifecycle state for ${action}`);
    await client.close();
  }
});

test("resume timeout leaves response and cleanup grace after the bounded companion operation", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const originalSetTimeout = globalThis.setTimeout;
  let resumeTimeout: number | undefined;
  globalThis.setTimeout = ((callback: Parameters<typeof setTimeout>[0], delay?: number) => {
    resumeTimeout = delay;
    return originalSetTimeout(callback, 3_600_000);
  }) as typeof globalThis.setTimeout;
  try {
    const pending = client.requestLifecycle("resume");
    assert.equal(resumeTimeout, 13 * 60_000);
    await client.close();
    assert.deepEqual(await pending, { ok: false, error: "unavailable" });
  } finally {
    globalThis.setTimeout = originalSetTimeout;
  }
});

test("repair timeout covers pinned download and runtime health checks", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const originalSetTimeout = globalThis.setTimeout;
  let repairTimeout: number | undefined;
  globalThis.setTimeout = ((callback: Parameters<typeof setTimeout>[0], delay?: number) => {
    repairTimeout = delay;
    return originalSetTimeout(callback, 3_600_000);
  }) as typeof globalThis.setTimeout;
  try {
    const pending = client.requestLifecycle("repair");
    assert.equal(repairTimeout, 9 * 60_000);
    await client.close();
    assert.deepEqual(await pending, { ok: false, error: "unavailable" });
  } finally {
    globalThis.setTimeout = originalSetTimeout;
  }
});

test("tray lifecycle requests use the app-wide scope without workspace sync", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number; version: string; action: string; scope: string };
    assert.deepEqual(request, { id: 1, version: "1", action: "state", scope: "desktop:lifecycle" });
    fake.stdout.write(`${JSON.stringify({
      id: request.id,
      ok: true,
      result: { installation_id: null, operating_system: "linux", runtime_available: true, cua_ready: false, cua_upgrade_required: false, native_executor_ready: false, gateway_connected: false, relay_active: false, relay_paused: true, user_paused: false },
    })}\n`);
  });
  const result = await client.requestLifecycle("state");
  assert.equal(result.ok, true);
  await client.close();
});

test("companion client synchronizes the initial Local Session scope before forwarding hosted state", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number; action: string; scope: string; local_session?: object };
    if (request.id === 1) {
      assert.deepEqual(request, { id: 1, version: "1", action: "sync", scope: "scope-a" });
      fake.stdout.write(`${JSON.stringify({ id: request.id, ok: true })}\n`);
      return;
    }
    assert.deepEqual(request, {
      id: 2,
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
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number; action: string };
    const reply = request.action === "sync"
      ? { id: request.id, ok: true }
      : { id: request.id, ok: false, error: "missing_harness" };
    fake.stdout.write(`${JSON.stringify(reply)}\n`);
  });
  const pending = client.requestLocalSession({ version: "1", action: "prepare", scope: "scope-a", persona_id: "persona_1", harness: "codex" });
  assert.deepEqual(await pending, { ok: false, error: "missing_harness" });
  await client.close();
});

test("new Local Session state does not restore a scope after a newer scope request", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const requests: Array<{ id: number; action: string; scope: string }> = [];
  let resolveFirstSync!: () => void;
  const firstSyncSent = new Promise<void>((resolve) => { resolveFirstSync = resolve; });
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number; action: string; scope: string };
    requests.push(request);
    if (request.id === 1) {
      resolveFirstSync();
      return;
    }
    const reply = request.action === "sync"
      ? { id: request.id, ok: true }
      : { id: request.id, ok: true, local_session: { ok: true, version: "2" } };
    fake.stdout.write(`${JSON.stringify(reply)}\n`);
  });

  const oldState = client.requestLocalSession({ version: "1", action: "state", scope: "scope-a" });
  await firstSyncSent;
  const newState = client.requestLocalSession({ version: "1", action: "state", scope: "scope-b" });
  fake.stdout.write('{"id":1,"ok":true}\n');

  assert.deepEqual(await oldState, { ok: false, error: "stale_request" });
  assert.deepEqual(await newState, { ok: true, version: "2" });
  assert.deepEqual(requests.map(({ action, scope }) => [action, scope]), [
    ["sync", "scope-a"],
    ["sync", "scope-b"],
    ["local_session", "scope-b"],
  ]);
  await client.close();
});

test("companion client resynchronizes after a stale scope acknowledgment", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const requests: Array<{ id: number; action: string; scope: string }> = [];
  let resolveScopeBSent!: () => void;
  const scopeBSent = new Promise<void>((resolve) => { resolveScopeBSent = resolve; });
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number; action: string; scope: string };
    requests.push(request);
    if (request.id === 2) {
      resolveScopeBSent();
      return;
    }
    fake.stdout.write(`${JSON.stringify({ id: request.id, ok: true })}\n`);
  });

  assert.deepEqual(await client.request({ version: "1", action: "sync", scope: "scope-a" }), { ok: true });
  const scopeB = client.request({ version: "1", action: "sync", scope: "scope-b" });
  await scopeBSent;
  const scopeA = client.request({ version: "1", action: "sync", scope: "scope-a" });
  fake.stdout.write('{"id":2,"ok":true}\n');

  assert.deepEqual(await scopeB, { ok: false, error: "stale_request" });
  assert.deepEqual(await scopeA, { ok: true });
  assert.deepEqual(requests.map(({ action, scope }) => [action, scope]), [
    ["sync", "scope-a"],
    ["sync", "scope-b"],
    ["sync", "scope-a"],
  ]);
  await client.close();
});

test("main-document invalidation syncs an empty scope and accepts canceled Local Session work", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const requests: Array<{ id: number; action: string; scope: string }> = [];
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number; action: string; scope: string };
    requests.push(request);
    if (request.id === 1 || request.action === "sync") fake.stdout.write(`${JSON.stringify({ id: request.id, ok: true })}\n`);
  });

  const pending = client.requestLocalSession({
    version: "1", action: "prepare", scope: "scope-a", persona_id: "persona_1", harness: "codex",
  });
  await new Promise<void>((resolve) => setImmediate(resolve));
  const invalidation = client.invalidatePageRequests();
  await new Promise<void>((resolve) => setImmediate(resolve));
  fake.stdout.write('{"id":2,"ok":false,"error":"stale_request"}\n');

  assert.deepEqual(await pending, { ok: false, error: "stale_request" });
  await invalidation;
  assert.deepEqual(requests.map(({ action, scope }) => [action, scope]), [
    ["sync", "scope-a"],
    ["local_session", "scope-a"],
    ["sync", ""],
  ]);
  await client.close();
});

test("main-document invalidation cannot be skipped by a new request in the same scope", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const requests: Array<{ id: number; action: string; scope: string }> = [];
  let resolveWorkSent!: () => void;
  const workSent = new Promise<void>((resolve) => { resolveWorkSent = resolve; });
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number; action: string; scope: string };
    requests.push(request);
    if (request.id === 2) {
      resolveWorkSent();
      return;
    }
    if (request.id === 3) fake.stdout.write('{"id":2,"ok":false,"error":"stale_request"}\n');
    const reply = request.action === "sync"
      ? { id: request.id, ok: true }
      : { id: request.id, ok: true, local_session: { ok: true, version: "2" } };
    fake.stdout.write(`${JSON.stringify(reply)}\n`);
  });

  const pendingWork = client.requestLocalSession({
    version: "1", action: "prepare", scope: "scope-a", persona_id: "persona_1", harness: "codex",
  });
  await workSent;
  const invalidation = client.invalidatePageRequests();
  const newPageState = client.requestLocalSession({ version: "1", action: "state", scope: "scope-a" });

  assert.deepEqual(await pendingWork, { ok: false, error: "stale_request" });
  await invalidation;
  assert.deepEqual(await newPageState, { ok: true, version: "2" });
  assert.deepEqual(requests.map(({ action, scope }) => [action, scope]), [
    ["sync", "scope-a"],
    ["local_session", "scope-a"],
    ["sync", ""],
    ["sync", "scope-a"],
    ["local_session", "scope-a"],
  ]);
  await client.close();
});

test("Desktop Control page commands wait behind main-document invalidation", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const requests: Array<{ id: number; action: string; scope: string }> = [];
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number; action: string; scope: string };
    requests.push(request);
    const result = request.action === "prepare"
      ? { id: request.id, ok: true, result: { installation_id: null, operating_system: "linux", runtime_available: false, cua_ready: false, cua_upgrade_required: false, native_executor_ready: false, gateway_connected: false, relay_paused: true, user_paused: false } }
      : { id: request.id, ok: true };
    fake.stdout.write(`${JSON.stringify(result)}\n`);
  });

  assert.deepEqual(await client.request({ version: "1", action: "sync", scope: "scope-a" }), { ok: true });
  const invalidation = client.invalidatePageRequests();
  const prepare = client.request({ version: "1", action: "prepare", scope: "scope-a", enrollment_ticket: "A".repeat(43) });

  await invalidation;
  assert.equal((await prepare).ok, true);
  assert.deepEqual(requests.map(({ action, scope }) => [action, scope]), [
    ["sync", "scope-a"],
    ["sync", ""],
    ["sync", "scope-a"],
    ["prepare", "scope-a"],
  ]);
  await client.close();
});

test("main-document invalidation reserves capacity beyond eight page requests", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const requests: Array<{ id: number; action: string; scope: string }> = [];
  fake.stdin.on("data", (chunk: Buffer) => {
    requests.push(JSON.parse(chunk.toString("utf8")) as { id: number; action: string; scope: string });
  });
  const pending = Array.from({ length: 8 }, () => client.requestLifecycle("state"));
  await new Promise<void>((resolve) => setImmediate(resolve));

  const invalidation = client.invalidatePageRequests();
  await new Promise<void>((resolve) => setImmediate(resolve));
  assert.equal(requests.length, 9);
  assert.deepEqual(requests[8], { id: 9, version: "1", action: "sync", scope: "" });

  await client.close();
  await invalidation;
  assert.deepEqual(await Promise.all(pending), Array.from({ length: 8 }, () => ({ ok: false, error: "unavailable" })));
});

test("companion client preserves finite session-lock failures", async () => {
  for (const error of ["session_locked", "session_state_unknown"] as const) {
    const fake = createFakeChild();
    const client = new CompanionClient(fake.child);
    fake.stdin.on("data", (chunk: Buffer) => {
      const request = JSON.parse(chunk.toString("utf8")) as { id: number; action: string };
      fake.stdout.write(`${JSON.stringify(request.action === "sync"
        ? { id: request.id, ok: true }
        : { id: request.id, ok: false, error })}\n`);
    });
    const pending = client.request({ version: "1", action: "state", scope: "workspace:a" });
    assert.deepEqual(await pending, { ok: false, error });
    await client.close();
  }
});

test("companion client preserves typed Cua update-required state", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const pending = client.requestLifecycle("state");
  fake.stdout.write('{"id":1,"ok":true,"result":{"installation_id":null,"operating_system":"linux","runtime_available":true,"cua_ready":false,"cua_upgrade_required":true,"native_executor_ready":false,"gateway_connected":false,"relay_active":false,"relay_paused":true,"user_paused":false}}\n');
  const result = await pending;
  assert.equal(result.ok, true);
  if (result.ok && "cua_upgrade_required" in result) assert.equal(result.cua_upgrade_required, true);
  else assert.fail("missing Cua update-required state");
  await client.close();
});

test("companion client rejects non-boolean Cua update-required state", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  const pending = client.requestLifecycle("state");
  fake.stdout.write('{"id":1,"ok":true,"result":{"installation_id":null,"operating_system":"linux","runtime_available":true,"cua_ready":false,"cua_upgrade_required":"yes","native_executor_ready":false,"gateway_connected":false,"relay_active":false,"relay_paused":true,"user_paused":false}}\n');
  assert.deepEqual(await pending, { ok: false, error: "unavailable" });
  assert.equal(client.isOpen, false);
  await client.close();
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
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number; action: string };
    fake.stdout.write(`${JSON.stringify({ id: request.id, ok: true })}\n`);
  });
  const pending = client.request({ version: "1", action: "state", scope: "" });
  assert.deepEqual(await pending, { ok: false, error: "unavailable" });
  assert.equal(client.isOpen, false);
});

test("prepare response includes a Linux platform and local readiness", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number; action: string };
    if (request.action === "sync") {
      fake.stdout.write(`${JSON.stringify({ id: request.id, ok: true })}\n`);
      return;
    }
    fake.stdout.write(`${JSON.stringify({
      id: request.id,
      ok: true,
      result: { installation_id: "install_01", operating_system: "linux", runtime_available: true, cua_ready: true, cua_upgrade_required: false, native_executor_ready: false, gateway_connected: false, relay_active: true, relay_paused: false, user_paused: false },
    })}\n`);
  });
  const result = await client.request({ version: "1", action: "prepare", scope: "workspace:request", enrollment_ticket: "A".repeat(43) });
  assert.deepEqual(result, {
    ok: true,
    installation_id: "install_01",
    cua_ready: true,
    cua_upgrade_required: false,
    native_executor_ready: false,
    gateway_connected: false,
    relay_active: true,
    relay_paused: false,
    user_paused: false,
    operating_system: "linux",
    runtime_available: true,
  });
  await client.close();
});

test("prepare preserves a finite unavailable-runtime result", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number; action: string };
    if (request.action === "sync") fake.stdout.write(`${JSON.stringify({ id: request.id, ok: true })}\n`);
    else fake.stdout.write(`${JSON.stringify({ id: request.id, ok: true, result: { installation_id: null, operating_system: "linux", runtime_available: false, cua_ready: false, cua_upgrade_required: false, native_executor_ready: false, gateway_connected: false, relay_active: false, relay_paused: true, user_paused: false } })}\n`);
  });
  const pending = client.request({ version: "1", action: "prepare", scope: "workspace:request", enrollment_ticket: "A".repeat(43) });
  assert.deepEqual(await pending, {
    ok: true,
    installation_id: null,
    operating_system: "linux",
    runtime_available: false,
    cua_ready: false,
    cua_upgrade_required: false,
    native_executor_ready: false,
    gateway_connected: false,
    relay_active: false,
    relay_paused: true,
    user_paused: false,
  });
  assert.equal(client.isOpen, true);
  await client.close();
});

test("companion client closes pending calls without replay", async () => {
  const fake = createFakeChild();
  const client = new CompanionClient(fake.child);
  fake.stdin.on("data", (chunk: Buffer) => {
    const request = JSON.parse(chunk.toString("utf8")) as { id: number; action: string };
    if (request.action === "sync") fake.stdout.write(`${JSON.stringify({ id: request.id, ok: true })}\n`);
    else fake.stdout.write(`${JSON.stringify({ id: request.id, ok: true, result: { installation_id: null, operating_system: "linux", runtime_available: false, cua_ready: false, cua_upgrade_required: false, native_executor_ready: false, gateway_connected: false, relay_active: false, relay_paused: true, user_paused: false } })}\n`);
  });
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
