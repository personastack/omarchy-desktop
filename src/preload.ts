import { contextBridge, ipcRenderer } from "electron";

import { isInternalPersonaStackURL } from "./security.js";

type NativeHandler = Readonly<{ postMessage(message: unknown): Promise<unknown> }>;

const invoke = (channel: string): NativeHandler => ({
  postMessage: (message: unknown) => ipcRenderer.invoke(channel, { generation, payload: message }),
});

const generation = ipcRenderer.sendSync("personastack:bridge:init");

contextBridge.exposeInMainWorld("webkit", {
  messageHandlers: {
    personastackConcern: {
      postMessage: (message: unknown) => ipcRenderer.send("personastack:concern", { generation, payload: message }),
    },
    personastackChat: invoke("personastack:chat"),
    personastackChatWindow: invoke("personastack:chat-window"),
    personastackStack: invoke("personastack:stack"),
    personastackDesktopControl: invoke("personastack:desktop-control"),
  },
});

document.addEventListener("click", (event) => {
  if (!event.isTrusted) return;
  const anchor = event.composedPath().find((target): target is HTMLAnchorElement => target instanceof HTMLAnchorElement);
  if (!anchor || anchor.href === "" || isInternalPersonaStackURL(anchor.href, new URL(window.location.href))) return;
  if (anchor.protocol !== "http:" && anchor.protocol !== "https:") return;
  event.preventDefault();
  void ipcRenderer.invoke("personastack:open-external", { generation, payload: anchor.href });
}, true);
