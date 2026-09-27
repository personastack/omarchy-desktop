export type WindowSize = readonly [width: number, height: number];
export type WindowPosition = readonly [x: number, y: number];

export interface WorkArea {
  readonly x: number;
  readonly y: number;
  readonly width: number;
  readonly height: number;
}

export interface ChatWindowSizer {
  isDestroyed(): boolean;
  getSize(): number[];
  setSize(width: number, height: number): void;
  setMinimumSize(width: number, height: number): void;
}

const COLLAPSED_SIZE: WindowSize = [72, 72];
const DEFAULT_EXPANDED_SIZE: WindowSize = [440, 640];

export function collapseChatSize(
  current: WindowSize,
  previousExpandedSize?: WindowSize,
): { size: WindowSize; restoreSize?: WindowSize } {
  const shouldRemember = current[0] > COLLAPSED_SIZE[0] || current[1] > COLLAPSED_SIZE[1];
  return shouldRemember
    ? { size: COLLAPSED_SIZE, restoreSize: current }
    : previousExpandedSize
      ? { size: COLLAPSED_SIZE, restoreSize: previousExpandedSize }
      : { size: COLLAPSED_SIZE };
}

export function expandedChatSize(saved?: WindowSize): WindowSize {
  return saved ?? DEFAULT_EXPANDED_SIZE;
}

export function applyChatWindowSizeAction(
  action: "collapse" | "expand",
  window: ChatWindowSizer,
  saved?: WindowSize,
): WindowSize | undefined {
  if (window.isDestroyed()) return saved;

  if (action === "collapse") {
    const [width = DEFAULT_EXPANDED_SIZE[0], height = DEFAULT_EXPANDED_SIZE[1]] = window.getSize();
    const collapsed = collapseChatSize([width, height], saved);
    window.setMinimumSize(...collapsed.size);
    window.setSize(...collapsed.size);
    return collapsed.restoreSize;
  }

  window.setMinimumSize(340, 360);
  const size = expandedChatSize(saved);
  window.setSize(...size);
  return size;
}

export function clampChatPosition(
  target: WindowPosition,
  size: WindowSize,
  workArea: WorkArea,
): WindowPosition {
  const maxX = Math.max(workArea.x, workArea.x + workArea.width - size[0]);
  const maxY = Math.max(workArea.y, workArea.y + workArea.height - size[1]);
  return [
    Math.min(Math.max(target[0], workArea.x), maxX),
    Math.min(Math.max(target[1], workArea.y), maxY),
  ];
}
