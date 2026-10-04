export interface HostedChatCloseWindow {
  executeJavaScript(script: string): Promise<unknown>;
}

export async function delegateChatWindowClose(
  _window: HostedChatCloseWindow,
  closeNativeWindow: () => void,
): Promise<void> {
  // X owns only the visible native window. Never invoke a hosted session close.
  closeNativeWindow();
}
