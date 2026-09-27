export function createAfterReady<T>(ready: Promise<unknown>, create: () => T): Promise<T> {
  return ready.then(create);
}
