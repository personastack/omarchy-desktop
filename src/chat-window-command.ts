import {
  applyChatWindowSizeAction,
  clampChatPosition,
  type ChatWindowSizer,
  type WorkArea,
  type WindowPosition,
} from "./chat-window-state.js";
import type { ChatWindowCommand } from "./security.js";

export interface ChatWindowCommandTarget extends ChatWindowSizer {
  minimize(): void;
  isAlwaysOnTop(): boolean;
  setAlwaysOnTop(flag: boolean, level?: "normal" | "floating" | "torn-off-menu" | "modal-panel" | "main-menu" | "status" | "pop-up-menu" | "screen-saver"): void;
  getPosition(): number[];
  setPosition(x: number, y: number): void;
}

export interface ChatWindowDisplayBounds {
  readonly x: number;
  readonly y: number;
  readonly width: number;
  readonly height: number;
}

export interface ChatWindowCommandDependencies {
  readonly close: () => void;
  readonly workAreaFor: (bounds: ChatWindowDisplayBounds) => WorkArea;
}

export function applyChatWindowCommand(
  command: ChatWindowCommand,
  window: ChatWindowCommandTarget,
  savedExpandedSize: readonly [number, number] | undefined,
  dependencies: ChatWindowCommandDependencies,
): readonly [number, number] | undefined {
  switch (command.action) {
    case "minimize":
      window.minimize();
      return savedExpandedSize;
    case "close":
      dependencies.close();
      return savedExpandedSize;
    case "collapse":
      return applyChatWindowSizeAction("collapse", window, savedExpandedSize);
    case "expand":
      return applyChatWindowSizeAction("expand", window, savedExpandedSize);
    case "pin":
      window.setAlwaysOnTop(!window.isAlwaysOnTop(), "floating");
      return savedExpandedSize;
    case "drag": {
      const [x = 0, y = 0] = window.getPosition();
      const [width = 440, height = 640] = window.getSize();
      const targetX = x + command.dx;
      const targetY = y + command.dy;
      const bounds = { x: targetX, y: targetY, width, height };
      const position: WindowPosition = clampChatPosition(
        [targetX, targetY],
        [width, height],
        dependencies.workAreaFor(bounds),
      );
      window.setPosition(...position);
      return savedExpandedSize;
    }
  }
}
