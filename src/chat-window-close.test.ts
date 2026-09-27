import assert from "node:assert/strict";
import test from "node:test";

import { delegateChatWindowClose, type HostedChatCloseWindow } from "./chat-window-close.js";

class WebContentsFake implements HostedChatCloseWindow {
  result: unknown = true;
  failure: Error | undefined;
  scripts: string[] = [];

  async executeJavaScript(script: string): Promise<unknown> {
    this.scripts.push(script);
    if (this.failure) throw this.failure;
    return this.result;
  }
}

test("native close delegates to the hosted API-backed close path", async () => {
  const contents = new WebContentsFake();
  let nativeCloseCount = 0;

  await delegateChatWindowClose(contents, () => { nativeCloseCount += 1; });

  assert.equal(contents.scripts.length, 1);
  assert.match(contents.scripts[0] ?? "", /personastackDesktopClose/);
  assert.equal(nativeCloseCount, 0);
});

test("native close falls back when the hosted close bridge is unavailable", async () => {
  const contents = new WebContentsFake();
  contents.result = false;
  let nativeCloseCount = 0;

  await delegateChatWindowClose(contents, () => { nativeCloseCount += 1; });

  assert.equal(nativeCloseCount, 1);
});

test("native close falls back when renderer execution fails", async () => {
  const contents = new WebContentsFake();
  contents.failure = new Error("renderer gone");
  let nativeCloseCount = 0;

  await delegateChatWindowClose(contents, () => { nativeCloseCount += 1; });

  assert.equal(nativeCloseCount, 1);
});
