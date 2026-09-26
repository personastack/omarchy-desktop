import { randomUUID } from "node:crypto";
import { spawn } from "node:child_process";
import { basename, join } from "node:path";

import {
  app,
  BrowserWindow,
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
} from "electron";

import {
  authorizeBridgeFrame,
  isNewConcernEvent,
  isAllowedOAuthPopupURL,
  isAllowedMainNavigation,
  isGoogleOAuthURL,
  isSafeExternalURL,
  isTrustedAppURL,
  isCurrentBridgeGeneration,
  parseChatMainCommand,
  parseChatWindowCommand,
  parseDesktopControlCommand,
  parseStackCommand,
  resolveAppURL,
  unwrapBridgePayload,
  type BridgeRole,
  type ChatMainCommand,
  type ChatWindowCommand,
  type StackCommand,
} from "./security.js";
import { CompanionClient, type DesktopControlResult } from "./companion-client.js";

const APP_NAME = "PersonaStack";
const APP_ICON = "data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHdpZHRoPSIzMiIgaGVpZ2h0PSIzMiIgdmlld0JveD0iMCAwIDMyIDMyIj48cmVjdCB3aWR0aD0iMzIiIGhlaWdodD0iMzIiIHJ4PSI4IiBmaWxsPSIjNzY1NUZGIi8+PHBhdGggZD0iTTEwIDguNWg4LjVhNS41IDUuNSAwIDAgMSAwIDExSDExdjQuNWwtNC01LjUgNC01LjVWMTMuNWg3LjVhMS41IDEuNSAwIDAgMCAwLTNIMTB6IiBmaWxsPSJ3aGl0ZSIvPjwvc3ZnPg==";

interface RegisteredWindow {
  readonly role: BridgeRole;
  readonly window: BrowserWindow;
  generation: number;
}

const appURL = resolveAppURL(process.argv, process.env.PERSONASTACK_DEFAULT_URL);
const sessionPartition = `persist:personastack:${encodeURIComponent(appURL.origin)}`;
const browserSession = session.fromPartition(sessionPartition);
const windows = new Map<number, RegisteredWindow>();
const chats = new Map<string, BrowserWindow>();
const stackWindows = new Map<string, BrowserWindow>();
const expandedChatSizes = new Map<BrowserWindow, readonly [number, number]>();
let mainWindow: BrowserWindow | undefined;
let tray: Tray | undefined;
let isQuitting = false;
let chatScope = "";
let companionClient: CompanionClient | undefined;

app.setName(APP_NAME);

if (!app.requestSingleInstanceLock()) {
  app.quit();
} else {
  app.on("second-instance", () => openMainWindow());

  app.on("before-quit", () => {
    isQuitting = true;
    companionClient?.close();
  });

  app.whenReady().then(() => {
    configureDownloads();
    createTray();
    openMainWindow();
  });
}

function appOriginURL(): URL {
  return new URL(appURL.href);
}

function createWindow(role: BridgeRole, options: Electron.BrowserWindowConstructorOptions): BrowserWindow {
  const window = new BrowserWindow({
    ...options,
    webPreferences: {
      partition: sessionPartition,
      preload: join(import.meta.dirname, "preload.js"),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
      webSecurity: true,
      allowRunningInsecureContent: false,
    },
  });
  const entry: RegisteredWindow = { role, window, generation: 0 };
  windows.set(window.webContents.id, entry);
  configureWebContents(entry);
  window.on("closed", () => windows.delete(window.webContents.id));
  return window;
}

function openMainWindow(): void {
  if (mainWindow && !mainWindow.isDestroyed()) {
    mainWindow.show();
    mainWindow.focus();
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
  mainWindow.once("ready-to-show", () => mainWindow?.show());
  mainWindow.on("close", (event) => {
    if (!isQuitting) {
      event.preventDefault();
      mainWindow?.hide();
    }
  });
  void mainWindow.loadURL(appOriginURL().href);
}

function configureWebContents(entry: RegisteredWindow): void {
  const contents = entry.window.webContents;
  contents.setWindowOpenHandler(({ url }) => {
    if (isGoogleOAuthURL(url)) {
      openGoogleOAuthWindow(url);
      return { action: "deny" };
    }
    return { action: "deny" };
  });
  contents.on("will-navigate", (event, targetURL) => {
    if (isAllowedMainNavigation(targetURL, appOriginURL())) return;
    event.preventDefault();
    if (entry.role === "main") invalidatePopouts();
  });
  contents.on("will-redirect", (event, targetURL) => {
    if (isAllowedMainNavigation(targetURL, appOriginURL())) return;
    event.preventDefault();
    if (entry.role === "main") invalidatePopouts();
  });
  contents.on("did-start-navigation", (_event, _url, isInPlace, isMainFrame) => {
    if (isMainFrame && !isInPlace) entry.generation += 1;
  });
  contents.on("did-navigate", (_event, rawURL) => {
    const path = new URL(rawURL).pathname;
    if (entry.role === "main" && (path === "/login" || path === "/logout")) invalidatePopouts();
  });
  contents.on("did-fail-load", (_event, _code, _description, _url, isMainFrame) => {
    if (!isMainFrame) return;
    if (entry.role === "main") invalidatePopouts();
    else closeWindow(entry.window);
  });
  contents.on("render-process-gone", () => {
    if (entry.role === "main") invalidatePopouts();
    else closeWindow(entry.window);
  });
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
    currentGeneration: isCurrentBridgeGeneration(rawGeneration, entry.generation),
  } as const;
  return authorizeBridgeFrame(identity, appOriginURL()) ? entry : undefined;
}

function registerBridgeHandlers(): void {
  ipcMain.on("personastack:bridge:init", (event) => {
    const entry = authorizeSender(event);
    event.returnValue = entry ? entry.generation : -1;
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

  ipcMain.handle("personastack:open-external", async (event, rawURL: unknown) => {
    const envelope = unwrapBridgePayload(rawURL);
    if (!envelope || !bridgeGeneration(event, envelope.generation) || !isSafeExternalURL(envelope.payload) ||
        isTrustedAppURL(envelope.payload, appOriginURL())) return { ok: false };
    await shell.openExternal(envelope.payload);
    return { ok: true };
  });
}

function getCompanionClient(): CompanionClient | undefined {
  if (process.platform !== "linux") return undefined;
  if (companionClient?.isOpen) return companionClient;
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
    child.once("error", clear);
    child.once("exit", clear);
    return client;
  } catch {
    return undefined;
  }
}

function openGoogleOAuthWindow(initialURL: string): void {
  if (!isGoogleOAuthURL(initialURL)) return;
  const popup = new BrowserWindow({
    width: 520,
    height: 700,
    show: false,
    title: "Sign in with Google",
    webPreferences: {
      partition: sessionPartition,
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
      webSecurity: true,
      allowRunningInsecureContent: false,
    },
  });
  popup.webContents.setWindowOpenHandler(() => ({ action: "deny" }));
  popup.webContents.on("will-navigate", (event, rawURL) => {
    if (!isAllowedOAuthPopupURL(rawURL, appOriginURL())) event.preventDefault();
  });
  popup.webContents.on("did-navigate", (_event, rawURL) => {
    if (isTrustedAppURL(rawURL, appOriginURL())) popup.close();
  });
  popup.webContents.on("will-redirect", (event, rawURL) => {
    if (!isAllowedOAuthPopupURL(rawURL, appOriginURL())) event.preventDefault();
  });
  popup.once("ready-to-show", () => popup.show());
  void popup.loadURL(initialURL).catch(() => popup.close());
}

function applyChatMainCommand(command: ChatMainCommand): void {
  if (command.action === "sync") {
    if (chatScope !== command.scope) {
      chatScope = command.scope;
      for (const chat of chats.values()) closeWindow(chat);
      chats.clear();
    }
    return;
  }
  if (command.scope !== chatScope) {
    chatScope = command.scope;
    for (const chat of chats.values()) closeWindow(chat);
    chats.clear();
  }
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
  const url = routeURL(appOriginURL().href, "/user/personas/chat/desktop-popout", { persona_id: personaID });
  void window.loadURL(url).then(() => window.show());
  return window;
}

function applyChatWindowCommand(command: ChatWindowCommand, window: BrowserWindow): void {
  switch (command.action) {
    case "minimize":
      window.minimize();
      return;
    case "close":
      closeWindow(window);
      return;
    case "collapse":
      if (!window.isDestroyed()) {
        const [width = 440, height = 640] = window.getSize();
        if (width > 72 || height > 72) expandedChatSizes.set(window, [width, height]);
        window.setSize(72, 72);
        window.setMinimumSize(72, 72);
      }
      return;
    case "expand":
      if (!window.isDestroyed()) {
        window.setMinimumSize(340, 360);
        const [width = 440, height = 640] = expandedChatSizes.get(window) ?? [440, 640];
        window.setSize(width, height);
      }
      return;
    case "pin":
      window.setAlwaysOnTop(!window.isAlwaysOnTop(), "floating");
      return;
    case "drag": {
      const [x = 0, y = 0] = window.getPosition();
      const [width = 440, height = 640] = window.getSize();
      const targetX = x + command.dx;
      const targetY = y + command.dy;
      const display = screen.getDisplayMatching({ x: targetX, y: targetY, width, height });
      const area = display.workArea;
      const maxX = Math.max(area.x, area.x + area.width - width);
      const maxY = Math.max(area.y, area.y + area.height - height);
      window.setPosition(
        Math.min(Math.max(targetX, area.x), maxX),
        Math.min(Math.max(targetY, area.y), maxY),
      );
    }
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
  window.on("closed", () => {
    if (stackWindows.get(key) === window) stackWindows.delete(key);
  });
  const url = isStack
    ? routeURL(appOriginURL().href, "/user/stacks/desktop-popout", { stack_id: command.stack_id, view: command.view })
    : routeURL(appOriginURL().href, "/user/personas/activity/desktop-popout", { persona_id: command.persona_id });
  void window.loadURL(url).then(() => window.show());
}

function routeURL(currentURL: string, path: string, query: Record<string, string>): string {
  const url = new URL(currentURL);
  url.pathname = path;
  url.search = new URLSearchParams(query).toString();
  url.hash = "";
  return url.href;
}

function invalidatePopouts(): void {
  for (const window of [...chats.values(), ...stackWindows.values()]) closeWindow(window);
  chats.clear();
  stackWindows.clear();
  chatScope = "";
}

function closeWindow(window: BrowserWindow): void {
  if (!window.isDestroyed()) window.close();
}

function createTray(): void {
  tray = new Tray(nativeImage.createFromDataURL(APP_ICON));
  tray.setToolTip(`${APP_NAME} is running`);
  tray.setContextMenu(Menu.buildFromTemplate([
    { label: "Open PersonaStack", click: openMainWindow },
    { type: "separator" },
    { label: "Quit", click: () => app.quit() },
  ]));
  tray.on("click", openMainWindow);
}

function configureDownloads(): void {
  browserSession.on("will-download", (_event, item: DownloadItem) => {
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
