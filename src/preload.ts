import { contextBridge, ipcRenderer } from "electron";

import { shouldKeepPersonaStackLinkInApp } from "./security.js";

type NativeHandler = Readonly<{ postMessage(message: unknown): Promise<unknown> }>;

const invoke = (channel: string): NativeHandler => ({
  postMessage: (message: unknown) => ipcRenderer.invoke(channel, { generation, payload: message }),
});

const generation = ipcRenderer.sendSync("personastack:bridge:init");
const configuredAppOrigin = ipcRenderer.sendSync("personastack:bridge:app-origin");
const configuredAppURL = typeof configuredAppOrigin === "string" && configuredAppOrigin !== ""
  ? new URL(configuredAppOrigin)
  : new URL(window.location.origin);

contextBridge.exposeInMainWorld("webkit", {
  messageHandlers: {
    personastackConcern: {
      postMessage: (message: unknown) => ipcRenderer.send("personastack:concern", { generation, payload: message }),
    },
    personastackChat: invoke("personastack:chat"),
    personastackChatWindow: invoke("personastack:chat-window"),
    personastackStack: invoke("personastack:stack"),
    personastackDesktopControl: invoke("personastack:desktop-control"),
    personastackLocalSession: invoke("personastack:local-session"),
  },
});

contextBridge.exposeInMainWorld("personastackDesktopPlatform", process.platform);

document.addEventListener("click", (event) => {
  if (!event.isTrusted) return;
  const anchor = event.composedPath().find((target): target is HTMLAnchorElement => target instanceof HTMLAnchorElement);
  if (!anchor || anchor.href === "" || shouldKeepPersonaStackLinkInApp(anchor.href, configuredAppURL)) return;
  if (anchor.protocol !== "http:" && anchor.protocol !== "https:") return;
  event.preventDefault();
  const channel = generation >= 0 ? "personastack:open-external" : "personastack:open-user-external";
  const payload = generation >= 0 ? { generation, payload: anchor.href } : anchor.href;
  void ipcRenderer.invoke(channel, payload);
}, true);
