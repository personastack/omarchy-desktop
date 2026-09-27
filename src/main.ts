import { randomUUID } from "node:crypto";
import { spawn } from "node:child_process";
import { basename, join } from "node:path";
import {
  app,
  BrowserWindow,
  dialog,
  ipcMain,
  Menu,
  nativeImage,
  Notification,
  screen,
  session,
  shell,
  Tray,
  type DownloadItem,
  type IpcMainEvent,
  type IpcMainInvokeEvent,
  type MenuItemConstructorOptions,
} from "electron";

import { getAutostartStatus, setAutostartEnabled, type AutostartStatus } from "./autostart.js";
import { createAfterReady } from "./ready-session.js";
import { beginDesktopAuthHandoff, consumeDesktopAuthHandoff, exchangeDesktopAuthHandoff, type PendingDesktopAuthHandoff } from "./desktop-auth-handoff.js";
import {
  beginExternalOAuthReturn,
  isCurrentExternalOAuthAttempt,
  markExternalOAuthBlurred,
  refreshExternalOAuthOnFocus,
  type PendingExternalOAuthReturn,
} from "./external-oauth-return.js";
import { delegateChatWindowClose } from "./chat-window-close.js";
import { applyChatWindowCommand as dispatchChatWindowCommand } from "./chat-window-command.js";
import { closePopoutWindows, synchronizePopoutScope } from "./popout-scope.js";
import { documentNavigationEffects, type DocumentNavigationEvent } from "./document-navigation.js";
import { loadAndShowWindow } from "./window-load.js";
import {
  beginNavigationGeneration,
  commitNavigationGeneration,
  rollbackNavigationGeneration,
  type NavigationGeneration,
} from "./navigation-generation.js";
import {
  authorizeBridgeFrame,
  handleFrameNavigation,
  googleIntegrationOAuthStartState,
  shouldStartInBackground,
  isNewConcernEvent,
  isEnterpriseOIDCStartURL,
  isTrustedPermissionRequest,
  isAllowedUserExternalLink,
  isGoogleOAuthURL,
  isSafeExternalURL,
  isTrustedAppURL,
  isCurrentBridgeGeneration,
  parseChatMainCommand,
  parseChatWindowCommand,
  parseDesktopControlCommand,
  parseLocalSessionCommand,
  parseStackCommand,
  resolveAppURL,
  unwrapBridgePayload,
  type BridgeRole,
  type ChatMainCommand,
  type ChatWindowCommand,
  type StackCommand,
} from "./security.js";
import {
  CompanionClient,
  type DesktopControlError,
  type DesktopControlResult,
  type LocalSessionError,
  type LocalSessionResult,
} from "./companion-client.js";
import {
  applyTrayActionResult,
  applyTrayStateRead,
  beginTrayControlAction,
  beginTrayStateRead,
  isCurrentTrayControlAction,
  sameTrayControlSnapshot,
  trayControlAction,
  trayControlCanDisconnect,
  trayControlCanRepair,
  trayControlCanSetUp,
  trayControlStatus,
  type TrayControlSnapshot,
} from "./tray-control.js";

const APP_NAME = "PersonaStack";
const APP_ICON = "data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHdpZHRoPSIzMiIgaGVpZ2h0PSIzMiIgdmlld0JveD0iMCAwIDMyIDMyIj48cmVjdCB3aWR0aD0iMzIiIGhlaWdodD0iMzIiIHJ4PSI4IiBmaWxsPSIjNzY1NUZGIi8+PHBhdGggZD0iTTEwIDguNWg4LjVhNS41IDUuNSAwIDAgMSAwIDExSDExdjQuNWwtNC01LjUgNC01LjVWMTMuNWg3LjVhMS41IDEuNSAwIDAgMCAwLTNIMTB6IiBmaWxsPSJ3aGl0ZSIvPjwvc3ZnPg==";

interface RegisteredWindow {
  readonly role: BridgeRole;
  readonly window: BrowserWindow;
  navigationGeneration: NavigationGeneration;
  pendingExternalOAuth?: PendingExternalOAuthReturn;
}

const appURL = resolveAppURL(process.argv, process.env.PERSONASTACK_DEFAULT_URL);
const sessionPartition = `persist:personastack:${encodeURIComponent(appURL.origin)}`;
const browserSession = createAfterReady(app.whenReady(), () => session.fromPartition(sessionPartition));
const windows = new Map<number, RegisteredWindow>();
const chats = new Map<string, BrowserWindow>();
const stackWindows = new Map<string, BrowserWindow>();
const expandedChatSizes = new Map<BrowserWindow, readonly [number, number]>();
const programmaticChatClose = new WeakSet<BrowserWindow>();
const closingWindows = new WeakSet<BrowserWindow>();
let mainWindow: BrowserWindow | undefined;
let tray: Tray | undefined;
let isQuitting = false;
let companionShutdownComplete = false;
let chatScope = "";
let companionClient: CompanionClient | undefined;
let launchAtLoginStatus: AutostartStatus | "unavailable" = "unavailable";
let trayControlSnapshot: TrayControlSnapshot = {};
let trayControlActionPending = false;
let trayControlRefreshing = false;
let trayControlRevision = 0;
let trayControlRefreshTimer: NodeJS.Timeout | undefined;
const launchInBackground = shouldStartInBackground(process.argv);
let pendingDesktopAuthHandoff: PendingDesktopAuthHandoff | undefined;
let externalOAuthAttemptId = 0;

app.setName(APP_NAME);

if (process.platform === "linux") {
  // Chat windows need Electron's programmatic move, resize and always-on-top support.
  app.commandLine.appendSwitch("ozone-platform", "x11");
}

if (!app.requestSingleInstanceLock()) {
  app.quit();
} else {
  app.on("second-instance", (_event, commandLine) => {
    const callbackURL = commandLine.find(isDesktopAuthHandoffURL);
    if (callbackURL) void completeDesktopAuthHandoff(callbackURL);
    openMainWindow();
  });
  app.on("open-url", (event, callbackURL) => {
    event.preventDefault();
    void completeDesktopAuthHandoff(callbackURL);
  });

  app.on("before-quit", (event) => {
    if (trayControlRefreshTimer) {
      clearInterval(trayControlRefreshTimer);
      trayControlRefreshTimer = undefined;
    }
    if (companionClient && !companionShutdownComplete) {
      event.preventDefault();
      void companionClient.close().then(() => {
        companionShutdownComplete = true;
        app.quit();
      });
      return;
    }
    isQuitting = true;
  });

  app.whenReady().then(async () => {
    const callbackURL = process.argv.find(isDesktopAuthHandoffURL);
    if (callbackURL) void completeDesktopAuthHandoff(callbackURL);
    const hostedSession = await browserSession;
    configureBrowserPermissions(hostedSession);
    configureDownloads(hostedSession);
    createTray();
    openMainWindow(!launchInBackground);
  });
}

function isDesktopAuthHandoffURL(value: string): boolean {
  return value.startsWith("personastack://");
}

function beginSystemBrowserSignIn(): void {
  invalidatePopouts();
  const launch = beginDesktopAuthHandoff(appOriginURL(), Date.now());
  pendingDesktopAuthHandoff = launch.attempt;
  void shell.openExternal(launch.url).catch(() => {
    if (pendingDesktopAuthHandoff?.attemptId === launch.attempt.attemptId) pendingDesktopAuthHandoff = undefined;
    dialog.showErrorBox("PersonaStack sign-in", "PersonaStack could not open the system browser. Try again.");
  });
}

async function completeDesktopAuthHandoff(callbackURL: string): Promise<void> {
  const consumption = consumeDesktopAuthHandoff(pendingDesktopAuthHandoff, callbackURL, Date.now());
  pendingDesktopAuthHandoff = consumption.pending;
  const callback = consumption.callback;
  if (!callback) return;

  try {
    const hostedSession = await browserSession;
    await exchangeDesktopAuthHandoff((input, init) => hostedSession.fetch(input, init), appOriginURL(), callback);
    openMainWindow(true, "/user/personas");
  } catch {
    openMainWindow(true, "/login");
    dialog.showErrorBox("PersonaStack sign-in", "Sign-in could not be completed. Return to PersonaStack and try again.");
  }
}

function appOriginURL(): URL {
  return new URL(appURL.href);
}

function configureBrowserPermissions(hostedSession: Electron.Session): void {
  hostedSession.setPermissionRequestHandler((contents, _permission, callback, details) => {
    const entry = windows.get(contents.id);
    callback(isTrustedPermissionRequest(entry?.role, details.isMainFrame, details.requestingUrl, undefined, appOriginURL()));
  });
  hostedSession.setPermissionCheckHandler((contents, _permission, requestingOrigin, details) => {
    const entry = contents ? windows.get(contents.id) : undefined;
    const requestingURL = details.requestingUrl ?? details.securityOrigin ?? requestingOrigin;
    return isTrustedPermissionRequest(entry?.role, details.isMainFrame, requestingURL, details.embeddingOrigin, appOriginURL());
  });
}

function createWindow(role: BridgeRole, options: Electron.BrowserWindowConstructorOptions): BrowserWindow {
  const window = new BrowserWindow({
    ...options,
    webPreferences: {
      partition: sessionPartition,
      preload: join(import.meta.dirname, "preload.cjs"),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
      webSecurity: true,
      allowRunningInsecureContent: false,
    },
  });
  const entry: RegisteredWindow = { role, window, navigationGeneration: { current: 0 } };
  windows.set(window.webContents.id, entry);
  window.on("focus", () => {
    if (entry.role !== "main") return;
    const refresh = refreshExternalOAuthOnFocus(entry.pendingExternalOAuth, window.webContents.getURL(), Date.now());
    entry.pendingExternalOAuth = refresh.pending;
    if (!refresh.returnURL || !isTrustedAppURL(refresh.returnURL, appOriginURL())) return;
    void window.loadURL(refresh.returnURL);
  });
  window.on("blur", () => {
    const pending = entry.pendingExternalOAuth;
    if (entry.role === "main" && pending) entry.pendingExternalOAuth = markExternalOAuthBlurred(pending);
  });
  configureWebContents(entry);
  window.on("closed", () => windows.delete(window.webContents.id));
  return window;
}

function openMainWindow(show = true, route?: string): void {
  if (mainWindow && !mainWindow.isDestroyed()) {
    if (route) void mainWindow.loadURL(new URL(route, appOriginURL()).href);
    if (show) {
      mainWindow.show();
      mainWindow.focus();
    }
    return;
  }
  mainWindow = createWindow("main", {
    width: 1280,
    height: 900,
    minWidth: 760,
    minHeight: 600,
    show: false,
    title: APP_NAME,
    icon: nativeImage.createFromDataURL(APP_ICON),
  });
  if (show) mainWindow.once("ready-to-show", () => mainWindow?.show());
  mainWindow.on("close", (event) => {
    if (!isQuitting) {
      event.preventDefault();
      mainWindow?.hide();
    }
  });
  void mainWindow.loadURL(route ? new URL(route, appOriginURL()).href : appOriginURL().href);
}

function configureWebContents(entry: RegisteredWindow): void {
  const contents = entry.window.webContents;
  contents.setWindowOpenHandler(({ url }) => {
    if (isGoogleOAuthURL(url)) {
      const appOrigin = appOriginURL();
      if (entry.role === "main" && googleIntegrationOAuthStartState(url, appOrigin)) {
        openGoogleServicesOAuth(entry, url);
      } else {
        beginSystemBrowserSignIn();
      }
      return { action: "deny" };
    }
    return { action: "deny" };
  });
  contents.on("will-frame-navigate", (event) => {
    authorizeFrameNavigation(entry, event, event.url, event.isMainFrame);
  });
  contents.on("will-redirect", (event, targetURL, _isInPlace, isMainFrame) => {
    authorizeFrameNavigation(entry, event, targetURL, isMainFrame);
  });
  contents.on("did-start-navigation", (_event, _rawURL, isInPlace, isMainFrame) => {
    if (!isMainFrame || isInPlace) return;
    entry.navigationGeneration = beginNavigationGeneration(entry.navigationGeneration);
    applyDocumentNavigationEffects(entry, "navigation-started");
  });
  contents.on("did-navigate", (_event, rawURL, httpResponseCode) => {
    entry.navigationGeneration = commitNavigationGeneration(entry.navigationGeneration);
    const navigatedURL = new URL(rawURL);
    if (entry.pendingExternalOAuth && (rawURL !== entry.pendingExternalOAuth.returnURL || httpResponseCode >= 400)) {
      entry.pendingExternalOAuth = undefined;
    }
    applyDocumentNavigationEffects(entry, "response-received", rawURL, httpResponseCode);
  });
  contents.on("did-fail-load", (_event, _code, _description, _url, isMainFrame) => {
    if (!isMainFrame) return;
    entry.navigationGeneration = rollbackNavigationGeneration(entry.navigationGeneration);
    applyDocumentNavigationEffects(entry, "load-failed");
  });
  contents.on("render-process-gone", () => {
    applyDocumentNavigationEffects(entry, "renderer-gone");
  });
}

function applyDocumentNavigationEffects(
  entry: RegisteredWindow,
  event: DocumentNavigationEvent,
  url = "",
  responseCode = 0,
): void {
  const effects = documentNavigationEffects(entry.role, event, url, responseCode);
  if (effects.cancelPageRequests && process.platform === "linux" && companionClient?.isOpen) {
    void companionClient.invalidatePageRequests();
  }
  if (effects.invalidatePopouts) invalidatePopouts();
  if (effects.closeWindow) closeWindow(entry.window);
}

function authorizeFrameNavigation(
  entry: RegisteredWindow,
  event: Readonly<{ preventDefault: () => void }>,
  targetURL: string,
  isMainFrame: boolean,
): void {
  const appOrigin = appOriginURL();
  if (isMainFrame) {
    if (entry.role === "main" && googleIntegrationOAuthStartState(targetURL, appOrigin)) {
      cancelMainFrameNavigation(entry, event);
      openGoogleServicesOAuth(entry, targetURL);
      return;
    }
    if (isGoogleOAuthURL(targetURL) || isEnterpriseOIDCStartURL(targetURL, appOrigin)) {
      cancelMainFrameNavigation(entry, event);
      beginSystemBrowserSignIn();
      return;
    }
  }
  const allowed = handleFrameNavigation(
    event,
    targetURL,
    entry.role,
    isMainFrame,
    appOrigin,
    invalidatePopouts,
  );
  if (!allowed && isMainFrame) {
    if (entry.role === "main") restoreNavigationGeneration(entry);
    else applyDocumentNavigationEffects(entry, "navigation-denied", targetURL);
  }
}

function openGoogleServicesOAuth(entry: RegisteredWindow, authorizationURL: string): void {
  const returnURL = entry.window.webContents.getURL();
  if (!isTrustedAppURL(returnURL, appOriginURL())) return;

  const attemptId = ++externalOAuthAttemptId;
  entry.pendingExternalOAuth = beginExternalOAuthReturn(attemptId, returnURL, Date.now() + 10 * 60_000);
  void shell.openExternal(authorizationURL).catch(() => {
    if (!isCurrentExternalOAuthAttempt(entry.pendingExternalOAuth, attemptId, entry.window.webContents.getURL())) return;
    entry.pendingExternalOAuth = undefined;
    dialog.showErrorBox("Google sign-in", "PersonaStack could not open the system browser. Try again.");
  });
}

function cancelMainFrameNavigation(
  entry: RegisteredWindow,
  event: Readonly<{ preventDefault: () => void }>,
): void {
  event.preventDefault();
  restoreNavigationGeneration(entry);
}

function restoreNavigationGeneration(entry: RegisteredWindow): void {
  entry.navigationGeneration = rollbackNavigationGeneration(entry.navigationGeneration);
}


function authorizeSender(event: IpcMainEvent | IpcMainInvokeEvent, roles?: readonly BridgeRole[]): RegisteredWindow | undefined {
  const entry = windows.get(event.sender.id);
  if (!entry || entry.window.webContents !== event.sender || (roles && !roles.includes(entry.role))) return undefined;
  const identity = {
    role: entry.role,
    registered: true,
    isMainFrame: event.senderFrame === event.sender.mainFrame,
    frameURL: event.senderFrame?.url ?? "",
    topFrameURL: event.sender.mainFrame.url,
    currentGeneration: true,
  } as const;
  return authorizeBridgeFrame(identity, appOriginURL()) ? entry : undefined;
}

function bridgeGeneration(event: IpcMainEvent | IpcMainInvokeEvent, rawGeneration: unknown, roles?: readonly BridgeRole[]): RegisteredWindow | undefined {
  if (typeof rawGeneration !== "number" || !Number.isSafeInteger(rawGeneration)) return undefined;
  const entry = windows.get(event.sender.id);
  if (!entry || (roles && !roles.includes(entry.role))) return undefined;
  const identity = {
    role: entry.role,
    registered: true,
    isMainFrame: event.senderFrame === event.sender.mainFrame,
    frameURL: event.senderFrame?.url ?? "",
    topFrameURL: event.sender.mainFrame.url,
    currentGeneration: isCurrentBridgeGeneration(rawGeneration, entry.navigationGeneration.current),
  } as const;
  return authorizeBridgeFrame(identity, appOriginURL()) ? entry : undefined;
}

function registerBridgeHandlers(): void {
  ipcMain.on("personastack:bridge:init", (event) => {
    const entry = authorizeSender(event);
    event.returnValue = entry ? entry.navigationGeneration.current : -1;
  });

  ipcMain.on("personastack:bridge:app-origin", (event) => {
    const entry = windows.get(event.sender.id);
    event.returnValue = entry?.window.webContents === event.sender &&
      event.senderFrame === event.sender.mainFrame ? appOriginURL().origin : "";
  });

  ipcMain.on("personastack:concern", (event, payload: unknown) => {
    const envelope = unwrapBridgePayload(payload);
    if (!envelope || !bridgeGeneration(event, envelope.generation, ["main"]) || !isNewConcernEvent(envelope.payload) || !Notification.isSupported()) return;
    new Notification({
      title: APP_NAME,
      body: "A new concern needs attention.",
      silent: false,
    }).show();
  });

  ipcMain.handle("personastack:chat", (event, payload: unknown) => {
    const envelope = unwrapBridgePayload(payload);
    const entry = envelope && bridgeGeneration(event, envelope.generation, ["main"]);
    const command = envelope && parseChatMainCommand(envelope.payload);
    if (!entry || !command) return { ok: false };
    applyChatMainCommand(command);
    return { ok: true };
  });

  ipcMain.handle("personastack:chat-window", (event, payload: unknown) => {
    const envelope = unwrapBridgePayload(payload);
    const entry = envelope && bridgeGeneration(event, envelope.generation, ["chat"]);
    const command = envelope && parseChatWindowCommand(envelope.payload);
    if (!entry || !command) return { ok: false };
    applyChatWindowCommand(command, entry.window);
    return { ok: true, collapsed: command.action === "collapse", pinned: entry.window.isAlwaysOnTop() };
  });

  ipcMain.handle("personastack:stack", (event, payload: unknown) => {
    const envelope = unwrapBridgePayload(payload);
    const entry = envelope && bridgeGeneration(event, envelope.generation, ["main"]);
    const command = envelope && parseStackCommand(envelope.payload);
    if (!entry || !command) return { ok: false };
    openStackWindow(command);
    return { ok: true };
  });

  ipcMain.handle("personastack:desktop-control", async (event, payload: unknown): Promise<DesktopControlResult> => {
    const envelope = unwrapBridgePayload(payload);
    const entry = envelope && bridgeGeneration(event, envelope.generation, ["main"]);
    const command = envelope && parseDesktopControlCommand(envelope.payload);
    if (!entry || !command) return { ok: false, error: "invalid_request" };
    const client = getCompanionClient();
    if (!client) {
      return { ok: false, error: process.platform === "linux" ? "unavailable" : "unsupported_platform" };
    }
    const result = await client.request(command);
    if (windows.get(event.sender.id) !== entry || bridgeGeneration(event, envelope.generation, ["main"]) !== entry) {
      return { ok: false, error: "stale_request" };
    }
    return result;
  });

  ipcMain.handle("personastack:local-session", async (event, payload: unknown): Promise<LocalSessionResult> => {
    const envelope = unwrapBridgePayload(payload);
    const entry = envelope && bridgeGeneration(event, envelope.generation, ["main"]);
    const command = envelope && parseLocalSessionCommand(envelope.payload);
    if (!entry || !command) throw new Error(localSessionFailure("invalid_request"));
    const client = getCompanionClient();
    if (!client) throw new Error(localSessionFailure("unavailable"));
    const result = await client.requestLocalSession(command);
    if (!result.ok) throw new Error(localSessionFailure(result.error));
    if (windows.get(event.sender.id) !== entry || bridgeGeneration(event, envelope.generation, ["main"]) !== entry) {
      throw new Error(localSessionFailure("stale_request"));
    }
    return result;
  });

  ipcMain.handle("personastack:open-external", async (event, rawURL: unknown) => {
    const envelope = unwrapBridgePayload(rawURL);
    if (!envelope || !bridgeGeneration(event, envelope.generation) || !isSafeExternalURL(envelope.payload) ||
        isTrustedAppURL(envelope.payload, appOriginURL())) return { ok: false };
    await shell.openExternal(envelope.payload);
    return { ok: true };
  });

  ipcMain.handle("personastack:open-user-external", async (event, rawURL: unknown) => {
    const entry = windows.get(event.sender.id);
    if (!entry || entry.window.webContents !== event.sender ||
        !isAllowedUserExternalLink(
          entry.role,
          true,
          event.senderFrame === event.sender.mainFrame,
          event.senderFrame?.url ?? "",
          rawURL,
          appOriginURL(),
        )) return { ok: false };
    await shell.openExternal(rawURL);
    return { ok: true };
  });
}

function getCompanionClient(): CompanionClient | undefined {
  if (process.platform !== "linux") return undefined;
  if (companionClient) return companionClient.isOpen ? companionClient : undefined;
  try {
    const binaryPath = app.isPackaged
      ? join(process.resourcesPath, "bin", "personastack-companion")
      : join(app.getAppPath(), "companion", "bin", "personastack-companion");
    const child = spawn(binaryPath, [appURL.origin], { stdio: ["pipe", "pipe", "ignore"] });
    const client = new CompanionClient(child);
    companionClient = client;
    const clear = (): void => {
      if (companionClient === client) companionClient = undefined;
    };
    child.once("exit", clear);
    child.once("close", clear);
    return client;
  } catch {
    return undefined;
  }
}

function localSessionFailure(error: LocalSessionError): string {
  switch (error) {
    case "invalid_request": return "The local session request is invalid.";
    case "invalid_bundle": return "The local session bundle is invalid. Try again.";
    case "stale_request": return "The page changed. Start the local session again.";
    case "missing_harness": return "Install the selected CLI, then try again.";
    case "outdated_harness": return "Update the selected CLI, then try again.";
    case "unsafe_files": return "PersonaStack cannot safely install the local session files.";
    case "unavailable": return "The local harness could not be configured. Try again.";
  }
}


function applyChatMainCommand(command: ChatMainCommand): void {
  if (command.action === "sync") {
    synchronizeHostedScope(command.scope);
    return;
  }
  synchronizeHostedScope(command.scope);
  const existing = chats.get(command.persona_id);
  if (existing && !existing.isDestroyed()) {
    existing.show();
    existing.focus();
    return;
  }
  const chat = createChatWindow(command.persona_id);
  expandedChatSizes.set(chat, [440, 640]);
  chats.set(command.persona_id, chat);
}

function createChatWindow(personaID: string): BrowserWindow {
  const window = createWindow("chat", {
    width: 440,
    height: 640,
    minWidth: 340,
    minHeight: 360,
    show: false,
    frame: false,
    transparent: true,
    resizable: true,
    title: "Persona chat",
    icon: nativeImage.createFromDataURL(APP_ICON),
  });
  window.on("closed", () => {
    expandedChatSizes.delete(window);
    if (chats.get(personaID) === window) chats.delete(personaID);
  });
  window.on("close", (event) => {
    if (programmaticChatClose.delete(window) || isQuitting) return;
    event.preventDefault();
    const entry = windows.get(window.webContents.id);
    if (entry?.role !== "chat" || !isCurrentChatDocument(window)) {
      closeWindow(window);
      return;
    }
    void delegateChatWindowClose(window.webContents, () => closeWindow(window));
  });
  const url = routeURL(appOriginURL().href, "/user/personas/chat/desktop-popout", { persona_id: personaID });
  showAfterPopoutLoad(window, url);
  return window;
}

function applyChatWindowCommand(command: ChatWindowCommand, window: BrowserWindow): void {
  const restoreSize = dispatchChatWindowCommand(command, window, expandedChatSizes.get(window), {
    close: () => closeWindow(window),
    workAreaFor: (bounds) => screen.getDisplayMatching(bounds).workArea,
  });
  if (command.action === "collapse" && restoreSize) {
    expandedChatSizes.set(window, restoreSize);
  }
}

function openStackWindow(command: StackCommand): void {
  const isStack = command.action === "open_stack_view";
  const key = isStack ? `${command.view}:${command.stack_id}` : `activity:${command.persona_id}`;
  const existing = stackWindows.get(key);
  if (existing && !existing.isDestroyed()) {
    existing.show();
    existing.focus();
    return;
  }
  const view = isStack ? command.view : "activity";
  const window = createWindow(view === "graph" ? "stack" : "activity", {
    width: 900,
    height: 700,
    show: false,
    frame: view !== "graph",
    transparent: view === "graph",
    resizable: true,
    title: isStack ? `Stack ${command.view}` : "Persona activity",
    icon: nativeImage.createFromDataURL(APP_ICON),
  });
  stackWindows.set(key, window);
  window.on("close", () => closingWindows.add(window));
  window.on("closed", () => {
    if (stackWindows.get(key) === window) stackWindows.delete(key);
  });
  const url = isStack
    ? routeURL(appOriginURL().href, "/user/stacks/desktop-popout", { stack_id: command.stack_id, view: command.view })
    : routeURL(appOriginURL().href, "/user/personas/activity/desktop-popout", { persona_id: command.persona_id });
  showAfterPopoutLoad(window, url);
}

function showAfterPopoutLoad(window: BrowserWindow, url: string): void {
  void loadAndShowWindow({
    load: () => window.loadURL(url),
    isCurrent: () => !window.isDestroyed() && !closingWindows.has(window) && windows.get(window.webContents.id)?.window === window,
    show: () => window.show(),
    close: () => closeWindow(window),
  });
}

function routeURL(currentURL: string, path: string, query: Record<string, string>): string {
  const url = new URL(currentURL);
  url.pathname = path;
  url.search = new URLSearchParams(query).toString();
  url.hash = "";
  return url.href;
}

function invalidatePopouts(): void {
  closePopoutWindows([chats, stackWindows], closeWindow);
  chatScope = "";
}

function synchronizeHostedScope(scope: string): void {
  chatScope = synchronizePopoutScope(
    chatScope,
    scope,
    [chats, stackWindows],
    closeWindow,
  );
}

function closeWindow(window: BrowserWindow): void {
  if (window.isDestroyed()) return;
  closingWindows.add(window);
  programmaticChatClose.add(window);
  window.close();
}

function isCurrentChatDocument(window: BrowserWindow): boolean {
  try {
    const current = new URL(window.webContents.getURL());
    return current.origin === appURL.origin && current.pathname === "/user/personas/chat/desktop-popout";
  } catch {
    return false;
  }
}

function createTray(): void {
  tray = new Tray(nativeImage.createFromDataURL(APP_ICON));
  tray.setToolTip(`${APP_NAME} is running`);
  updateTrayMenu();
  if (app.isPackaged) void refreshLaunchAtLoginStatus();
  if (process.platform === "linux") {
    void refreshTrayControlState();
    trayControlRefreshTimer = setInterval(() => { void refreshTrayControlState(); }, 5_000);
  }
  tray.on("click", () => openMainWindow());
}

function updateTrayMenu(): void {
  if (!tray) return;
  const items: MenuItemConstructorOptions[] = [
    { label: "Open PersonaStack", click: () => openMainWindow() },
  ];
  if (process.platform === "linux") {
    const visibleError = trayControlSnapshot.actionError ?? trayControlSnapshot.refreshError;
    items.push({ label: trayControlStatus(trayControlSnapshot), enabled: false });
    if (visibleError) items.push({ label: trayControlFailure(visibleError), enabled: false });
    const action = trayControlAction(trayControlSnapshot);
    if (action) {
      items.push({
        label: action === "resume" ? "Resume Remote Control" : "Pause Remote Control",
        enabled: !trayControlActionPending,
        click: () => { void runTrayLifecycleAction(action); },
      });
    } else if (trayControlCanSetUp(trayControlSnapshot)) {
      items.push({ label: "Set Up Desktop Control", click: () => openMainWindow(true, "/user/desktop-control") });
    }
    if (trayControlCanRepair(trayControlSnapshot)) {
      items.push({
        label: "Repair Cua Service",
        enabled: !trayControlActionPending,
        click: () => { void runTrayLifecycleAction("repair"); },
      });
    }
    if (trayControlCanDisconnect(trayControlSnapshot)) {
      items.push({
        label: "Disconnect this computer…",
        enabled: !trayControlActionPending,
        click: () => { void confirmTrayDisconnect(); },
      });
    }
    items.push({ type: "separator" });
  } else {
    items.push({ type: "separator" });
  }
  if (app.isPackaged) {
    items.push({
      label: launchAtLoginStatus === "conflict" ? "Launch at Login (manual entry found)" : "Launch at Login",
      type: "checkbox",
      checked: launchAtLoginStatus === "enabled",
      enabled: launchAtLoginStatus !== "conflict" && launchAtLoginStatus !== "unavailable",
      click: () => { void toggleLaunchAtLogin(); },
    });
    items.push({ type: "separator" });
  }
  items.push({ label: "Quit PersonaStack", click: () => app.quit() });
  tray.setContextMenu(Menu.buildFromTemplate(items));
}

async function refreshTrayControlState(): Promise<void> {
  if (trayControlRefreshing || trayControlActionPending || !tray || process.platform !== "linux") return;
  trayControlRefreshing = true;
  const revision = trayControlRevision;
  const previous = trayControlSnapshot;
  trayControlSnapshot = beginTrayStateRead(trayControlSnapshot);
  try {
    const client = getCompanionClient();
    if (!client) {
      if (revision === trayControlRevision) {
        trayControlSnapshot = { ...trayControlSnapshot, stateFresh: false, refreshError: "unavailable" };
      }
      return;
    }
    const result = await client.requestLifecycle("state");
    if (revision !== trayControlRevision) return;
    trayControlSnapshot = applyTrayStateRead(trayControlSnapshot, result);
  } catch {
    if (revision === trayControlRevision) {
      trayControlSnapshot = { ...trayControlSnapshot, stateFresh: false, refreshError: "unavailable" };
    }
  } finally {
    trayControlRefreshing = false;
    if (revision !== trayControlRevision) {
      void refreshTrayControlState();
    } else if (!sameTrayControlSnapshot(previous, trayControlSnapshot)) {
      updateTrayMenu();
    }
  }
}

async function confirmTrayDisconnect(): Promise<void> {
  if (trayControlActionPending || !tray || !trayControlCanDisconnect(trayControlSnapshot)) return;
  const result = await dialog.showMessageBox({
    type: "warning",
    title: "Disconnect this computer?",
    message: "This revokes Desktop Control for this computer in every workspace.",
    detail: "Active remote sessions will stop. You can set up this computer again later.",
    buttons: ["Disconnect", "Cancel"],
    defaultId: 1,
    cancelId: 1,
    noLink: true,
  });
  if (result.response === 0) await runTrayLifecycleAction("disconnect");
}

async function runTrayLifecycleAction(action: "pause" | "resume" | "repair" | "disconnect"): Promise<void> {
  if (trayControlActionPending || !tray) return;
  const current = action === "disconnect" || action === "repair"
    ? action === "disconnect" ? trayControlCanDisconnect(trayControlSnapshot) : trayControlCanRepair(trayControlSnapshot)
    : isCurrentTrayControlAction(trayControlSnapshot, action);
  if (!current) {
    trayControlSnapshot = { ...trayControlSnapshot, actionError: "unavailable" };
    updateTrayMenu();
    return;
  }
  trayControlRevision++;
  trayControlActionPending = true;
  trayControlSnapshot = beginTrayControlAction(trayControlSnapshot);
  updateTrayMenu();
  try {
    const client = getCompanionClient();
    if (!client) {
      trayControlSnapshot = { ...trayControlSnapshot, actionError: "unavailable" };
      return;
    }
    const result = await client.requestLifecycle(action);
    trayControlSnapshot = applyTrayActionResult(trayControlSnapshot, result);
  } catch {
    trayControlSnapshot = { ...trayControlSnapshot, stateFresh: false, actionError: "unavailable" };
  } finally {
    trayControlActionPending = false;
    updateTrayMenu();
    void refreshTrayControlState();
  }
}

function trayControlFailure(error: DesktopControlError): string {
  switch (error) {
    case "not_enrolled": return "Set up Desktop Control in PersonaStack.";
    case "keyring_unavailable": return "Linux Secret Service is unavailable.";
    case "session_locked": return "Unlock the Omarchy session, then resume remote control.";
    case "session_state_unknown": return "The session lock state could not be checked.";
    case "rejected": return "PersonaStack rejected the Desktop Control request.";
    default: return "Desktop Control could not complete the request.";
  }
}

async function refreshLaunchAtLoginStatus(): Promise<void> {
  try {
    launchAtLoginStatus = await getAutostartStatus(join(app.getPath("appData"), "autostart"));
  } catch {
    launchAtLoginStatus = "unavailable";
  }
  updateTrayMenu();
}

async function toggleLaunchAtLogin(): Promise<void> {
  if (!app.isPackaged || launchAtLoginStatus === "conflict" || launchAtLoginStatus === "unavailable") return;
  const directory = join(app.getPath("appData"), "autostart");
  try {
    await setAutostartEnabled(directory, launchAtLoginStatus !== "enabled");
    await refreshLaunchAtLoginStatus();
  } catch {
    await refreshLaunchAtLoginStatus();
    dialog.showErrorBox("Launch at Login", "PersonaStack could not update its login item. Check the user autostart folder and try again.");
  }
}

function configureDownloads(hostedSession: Electron.Session): void {
  hostedSession.on("will-download", (_event, item: DownloadItem) => {
    const fileName = basename(item.getFilename().trim()) || "download";
    const extension = fileName.includes(".") ? fileName.slice(fileName.lastIndexOf(".")) : "";
    const stem = extension ? fileName.slice(0, -extension.length) : fileName;
    item.setSavePath(join(app.getPath("downloads"), `${stem}-${randomUUID().slice(0, 8)}${extension}`));
  });
}

registerBridgeHandlers();

app.on("window-all-closed", () => {
  // The tray-owned app lifetime keeps the authenticated concern receiver available.
});
