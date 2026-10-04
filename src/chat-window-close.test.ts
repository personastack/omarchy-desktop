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

test("native close retires locally without invoking hosted JavaScript", async () => {
  const contents = new WebContentsFake();
  let nativeCloseCount = 0;

  await delegateChatWindowClose(contents, () => { nativeCloseCount += 1; });

  assert.equal(contents.scripts.length, 0);
  assert.equal(nativeCloseCount, 1);
});

test("native close does not require a hosted close bridge", async () => {
  const contents = new WebContentsFake();
  contents.result = false;
  let nativeCloseCount = 0;

  await delegateChatWindowClose(contents, () => { nativeCloseCount += 1; });

  assert.equal(contents.scripts.length, 0);
  assert.equal(nativeCloseCount, 1);
});

test("native close does not execute an unresponsive renderer", async () => {
  const contents = new WebContentsFake();
  contents.failure = new Error("renderer gone");
  let nativeCloseCount = 0;

  await delegateChatWindowClose(contents, () => { nativeCloseCount += 1; });

  assert.equal(contents.scripts.length, 0);
  assert.equal(nativeCloseCount, 1);
});
