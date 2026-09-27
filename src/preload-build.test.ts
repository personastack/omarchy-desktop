import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { Script, runInNewContext } from "node:vm";
import test from "node:test";

test("built sandbox preload is one CommonJS file and installs the finite bridge", async () => {
  const source = readFileSync(new URL("./preload.cjs", import.meta.url), "utf8");
  assert.doesNotThrow(() => new Script(source));
  assert.doesNotMatch(source, /^\s*import\s/m);
  assert.doesNotMatch(source, /require\(["']\.\//);

  const exposed: Record<string, unknown> = {};
  const invoked: Array<readonly [string, unknown]> = [];
  const listeners = new Map<string, (event: ClickEvent) => void>();
  class Anchor {
    readonly protocol: string;

    constructor(readonly href = "https://external.example/docs", readonly target = "") {
      this.protocol = new URL(href).protocol;
    }
  }
  interface ClickEvent {
    readonly isTrusted: boolean;
    composedPath(): unknown[];
    preventDefault(): void;
  }
  const electron = {
    contextBridge: {
      exposeInMainWorld: (name: string, value: unknown) => { exposed[name] = value; },
    },
    ipcRenderer: {
      sendSync: (channel: string) => channel === "personastack:bridge:init" ? 7 : "https://my.personastack.ai",
      send: () => undefined,
      invoke: (channel: string, payload: unknown) => {
        invoked.push([channel, payload]);
        return Promise.resolve({ ok: true });
      },
    },
  };
  const context = {
    require: (name: string) => {
      assert.equal(name, "electron");
      return electron;
    },
    URL,
    window: { location: { origin: "https://my.personastack.ai" } },
    document: { addEventListener: (name: string, listener: (event: ClickEvent) => void) => { listeners.set(name, listener); } },
    HTMLAnchorElement: Anchor,
    process: { platform: "linux" },
  };

  runInNewContext(source, context);

  assert.deepEqual(Object.keys(exposed).sort(), ["personastackDesktopPlatform", "webkit"]);
  assert.equal(exposed.personastackDesktopPlatform, "linux");
  const click = listeners.get("click");
  assert.ok(click);
  let prevented = false;
  click({ isTrusted: true, composedPath: () => [new Anchor()], preventDefault: () => { prevented = true; } });
  assert.equal(prevented, true);
  assert.equal(invoked.length, 1);
  assert.equal(invoked[0]?.[0], "personastack:open-external");
  assert.equal(JSON.stringify(invoked[0]?.[1]), JSON.stringify({ generation: 7, payload: "https://external.example/docs" }));

  let internalDefaultPrevented = false;
  click({
    isTrusted: true,
    composedPath: () => [new Anchor("https://my.personastack.ai/privacy", "_blank")],
    preventDefault: () => { internalDefaultPrevented = true; },
  });
  assert.equal(internalDefaultPrevented, true);
  assert.equal(invoked[1]?.[0], "personastack:open-new-context");
  assert.equal(JSON.stringify(invoked[1]?.[1]), JSON.stringify({ generation: 7, payload: "https://my.personastack.ai/privacy" }));

  let internalInAppPrevented = false;
  click({
    isTrusted: true,
    composedPath: () => [new Anchor("https://my.personastack.ai/user/personas", "_self")],
    preventDefault: () => { internalInAppPrevented = true; },
  });
  assert.equal(internalInAppPrevented, false);
  assert.equal(invoked.length, 2);

  let mailtoPrevented = false;
  click({
    isTrusted: true,
    composedPath: () => [new Anchor("mailto:support@personastack.ai", "_blank")],
    preventDefault: () => { mailtoPrevented = true; },
  });
  assert.equal(mailtoPrevented, true);
  assert.equal(invoked[2]?.[0], "personastack:open-new-context");
  assert.equal(JSON.stringify(invoked[2]?.[1]), JSON.stringify({ generation: 7, payload: "mailto:support@personastack.ai" }));

  let mailtoDefaultPrevented = false;
  click({
    isTrusted: true,
    composedPath: () => [new Anchor("mailto:support@personastack.ai")],
    preventDefault: () => { mailtoDefaultPrevented = true; },
  });
  assert.equal(mailtoDefaultPrevented, true);
  assert.equal(invoked[3]?.[0], "personastack:open-new-context");
  assert.equal(JSON.stringify(invoked[3]?.[1]), JSON.stringify({ generation: 7, payload: "mailto:support@personastack.ai" }));
});
