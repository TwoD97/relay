import { useSyncExternalStore } from "react";

export const DEFAULT_TERMINAL_FONT_SIZE = 14;
export const MIN_TERMINAL_FONT_SIZE = 10;
export const MAX_TERMINAL_FONT_SIZE = 24;
const key = "relay.terminal.fontSize.v1";
const valid = (value: number) => Number.isInteger(value) && value >= MIN_TERMINAL_FONT_SIZE && value <= MAX_TERMINAL_FONT_SIZE;
function readFontSize() {
  try {
    const value = Number(localStorage.getItem(key));
    if (valid(value)) return value;
  } catch { /* A blocked preference store must not block the terminal. */ }
  return DEFAULT_TERMINAL_FONT_SIZE;
}
let fontSize = readFontSize();
const subscribers = new Set<() => void>();
const notify = () => subscribers.forEach((subscriber) => subscriber());
function storageChanged(event: StorageEvent) {
  if (event.key !== key && event.key !== null) return;
  try { if (event.storageArea !== localStorage) return; } catch { return; }
  fontSize = readFontSize();
  notify();
}
function subscribe(subscriber: () => void) {
  if (!subscribers.size) window.addEventListener("storage", storageChanged);
  subscribers.add(subscriber);
  return () => {
    subscribers.delete(subscriber);
    if (!subscribers.size) window.removeEventListener("storage", storageChanged);
  };
}
export function setTerminalFontSize(value: number) {
  if (!valid(value) || value === fontSize) return;
  fontSize = value;
  try { localStorage.setItem(key, String(value)); } catch { /* Keep this window's usable preference. */ }
  notify();
}
export function useTerminalFontSize() {
  return useSyncExternalStore(subscribe, () => fontSize, () => DEFAULT_TERMINAL_FONT_SIZE);
}
