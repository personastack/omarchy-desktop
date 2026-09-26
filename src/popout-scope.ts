export interface PopoutRegistry<T> {
  values(): IterableIterator<T>;
  clear(): void;
}

export function closePopoutWindows<T>(registries: readonly PopoutRegistry<T>[], close: (window: T) => void): void {
  for (const registry of registries) {
    for (const window of registry.values()) close(window);
    registry.clear();
  }
}

export function synchronizePopoutScope<T>(
  currentScope: string,
  nextScope: string,
  registries: readonly PopoutRegistry<T>[],
  close: (window: T) => void,
): string {
  if (currentScope === nextScope) return currentScope;
  closePopoutWindows(registries, close);
  return nextScope;
}
