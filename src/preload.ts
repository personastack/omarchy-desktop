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
  if (!anchor || anchor.href === "") return;
  const opensNewContext = anchor.target.toLowerCase() === "_blank";
  const isMailtoLink = anchor.protocol === "mailto:";
  if (!opensNewContext && shouldKeepPersonaStackLinkInApp(anchor.href, configuredAppURL)) return;
  if (!isMailtoLink && anchor.protocol !== "http:" && anchor.protocol !== "https:") return;
  event.preventDefault();
  const channel = generation < 0
    ? "personastack:open-user-external"
    : opensNewContext || isMailtoLink ? "personastack:open-new-context" : "personastack:open-external";
  const payload = generation >= 0 ? { generation, payload: anchor.href } : anchor.href;
  void ipcRenderer.invoke(channel, payload);
}, true);
