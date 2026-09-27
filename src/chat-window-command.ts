import {
  applyChatWindowSizeAction,
  clampChatPosition,
  type ChatWindowSizer,
  type WindowPosition,
  type WorkAreaFor,
} from "./chat-window-state.js";
import type { ChatWindowCommand } from "./security.js";

export interface ChatWindowCommandTarget extends ChatWindowSizer {
  minimize(): void;
  isAlwaysOnTop(): boolean;
  setAlwaysOnTop(flag: boolean, level?: "normal" | "floating" | "torn-off-menu" | "modal-panel" | "main-menu" | "status" | "pop-up-menu" | "screen-saver"): void;
}

export interface ChatWindowCommandDependencies {
  readonly close: () => void;
  readonly workAreaFor: WorkAreaFor;
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
      return applyChatWindowSizeAction("collapse", window, savedExpandedSize, dependencies.workAreaFor);
    case "expand":
      return applyChatWindowSizeAction("expand", window, savedExpandedSize, dependencies.workAreaFor);
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
