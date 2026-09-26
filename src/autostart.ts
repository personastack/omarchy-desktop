import { lstat, mkdir, readFile, unlink, writeFile } from "node:fs/promises";
import { join } from "node:path";

const DESKTOP_FILE_NAME = "personastack.desktop";
const DESKTOP_FILE = [
  "[Desktop Entry]",
  "Type=Application",
  "Name=PersonaStack",
  "Comment=Keep PersonaStack available in the background",
  "Exec=personastack --background",
  "Icon=personastack",
  "Terminal=false",
  "NoDisplay=true",
  "X-GNOME-Autostart-enabled=true",
  "StartupNotify=false",
  "",
].join("\n");

export type AutostartStatus = "enabled" | "disabled" | "conflict";

export async function getAutostartStatus(directory: string): Promise<AutostartStatus> {
  const path = join(directory, DESKTOP_FILE_NAME);
  try {
    const info = await lstat(path);
    if (!info.isFile()) return "conflict";
    return (await readFile(path, "utf8")) === DESKTOP_FILE ? "enabled" : "conflict";
  } catch (error) {
    if (hasCode(error, "ENOENT")) return "disabled";
    throw error;
  }
}

export async function setAutostartEnabled(directory: string, enabled: boolean): Promise<void> {
  const path = join(directory, DESKTOP_FILE_NAME);
  if (enabled) {
    await mkdir(directory, { recursive: true, mode: 0o700 });
    const status = await getAutostartStatus(directory);
    if (status === "enabled") return;
    if (status === "conflict") throw new Error("A different PersonaStack login item already exists.");
    await writeFile(path, DESKTOP_FILE, { encoding: "utf8", flag: "wx", mode: 0o600 });
    return;
  }
  const status = await getAutostartStatus(directory);
  if (status === "disabled") return;
  if (status === "conflict") throw new Error("The existing login item is not owned by this application.");
  await unlink(path);
}

function hasCode(error: unknown, code: string): boolean {
  return typeof error === "object" && error !== null && "code" in error && error.code === code;
}
