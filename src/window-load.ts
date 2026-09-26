export interface PendingWindowLoad {
  load(): Promise<unknown>;
  isCurrent(): boolean;
  show(): void;
  close(): void;
}

export async function loadAndShowWindow(window: PendingWindowLoad): Promise<void> {
  try {
    await window.load();
    if (window.isCurrent()) window.show();
  } catch {
    window.close();
  }
}
