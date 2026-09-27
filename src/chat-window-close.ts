export interface HostedChatCloseWindow {
  executeJavaScript(script: string): Promise<unknown>;
}

const HOSTED_CLOSE_REQUEST =
  "typeof window.personastackDesktopClose === 'function' ? (window.personastackDesktopClose(), true) : false";

export async function delegateChatWindowClose(
  window: HostedChatCloseWindow,
  closeNativeWindow: () => void,
): Promise<void> {
  try {
    const requested = await window.executeJavaScript(HOSTED_CLOSE_REQUEST);
    if (requested === true) return;
  } catch {
    // A failed renderer call leaves no hosted owner able to finish the close.
  }
  closeNativeWindow();
}
