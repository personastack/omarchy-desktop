import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, rm, stat, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { getAutostartStatus, setAutostartEnabled } from "./autostart.js";

test("autostart enable writes one private background entry and disable removes only that entry", async () => {
  const root = await mkdtemp(join(tmpdir(), "personastack-autostart-"));
  const directory = join(root, "config", "autostart");
  try {
    assert.equal(await getAutostartStatus(directory), "disabled");
    await setAutostartEnabled(directory, true);
    assert.equal(await getAutostartStatus(directory), "enabled");
    const path = join(directory, "personastack.desktop");
    assert.match(await readFile(path, "utf8"), /Exec=personastack --background/);
    assert.equal((await stat(path)).mode & 0o777, 0o600);
    await setAutostartEnabled(directory, true);
    await setAutostartEnabled(directory, false);
    assert.equal(await getAutostartStatus(directory), "disabled");
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("autostart preserves a foreign entry and refuses to remove it", async () => {
  const root = await mkdtemp(join(tmpdir(), "personastack-autostart-"));
  const directory = join(root, "autostart");
  const path = join(directory, "personastack.desktop");
  try {
    await mkdir(directory);
    await writeFile(path, "[Desktop Entry]\nName=User entry\n");
    assert.equal(await getAutostartStatus(directory), "conflict");
    await assert.rejects(setAutostartEnabled(directory, true), /different PersonaStack login item/);
    await assert.rejects(setAutostartEnabled(directory, false), /not owned by this application/);
    assert.equal(await readFile(path, "utf8"), "[Desktop Entry]\nName=User entry\n");
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("autostart refuses to follow a foreign symlink", async () => {
  const root = await mkdtemp(join(tmpdir(), "personastack-autostart-"));
  const directory = join(root, "autostart");
  const target = join(root, "user-owned.desktop");
  try {
    await mkdir(directory);
    await writeFile(target, "user data\n");
    await symlink(target, join(directory, "personastack.desktop"));
    assert.equal(await getAutostartStatus(directory), "conflict");
    await assert.rejects(setAutostartEnabled(directory, false), /not owned by this application/);
    assert.equal(await readFile(target, "utf8"), "user data\n");
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
