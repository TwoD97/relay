import { useSyncExternalStore } from "react";

export type AppearancePreference = "system" | "light" | "dark";
export type ResolvedAppearance = "light" | "dark";
type Appearance = { preference: AppearancePreference; resolved: ResolvedAppearance };
type Snapshot = Appearance & { busy: boolean; error: string | null };
type DesktopWindow = Window & {
  __RELAY_APPEARANCE__?: unknown;
  __TAURI_INTERNALS__?: { invoke: (command: string, args?: Record<string, unknown>) => Promise<unknown> };
};

const storageKey = "relay.appearance.v1";
const media = window.matchMedia("(prefers-color-scheme: dark)");
const desktop = window as DesktopWindow;
const subscribers = new Set<() => void>();
const isPreference = (value: unknown): value is AppearancePreference => value === "system" || value === "light" || value === "dark";
const systemAppearance = (): ResolvedAppearance => media.matches ? "dark" : "light";
const resolve = (preference: AppearancePreference): ResolvedAppearance => preference === "system" ? systemAppearance() : preference;
function validAppearance(value: unknown): value is Appearance {
  if (!value || typeof value !== "object") return false;
  const item = value as Partial<Appearance>;
  return isPreference(item.preference) && (item.resolved === "light" || item.resolved === "dark") && (item.preference === "system" || item.preference === item.resolved);
}
function initialAppearance(): Appearance {
  if (validAppearance(desktop.__RELAY_APPEARANCE__)) return desktop.__RELAY_APPEARANCE__;
  let preference: AppearancePreference = "system";
  try { const saved = localStorage.getItem(storageKey); if (isPreference(saved)) preference = saved; } catch { /* Storage is optional for the embedded UI. */ }
  return { preference, resolved: resolve(preference) };
}
let snapshot: Snapshot = { ...initialAppearance(), busy: false, error: null };
let initialized = false;
let revision = 0;
let nativeEventVersion = 0;
function apply() {
  document.documentElement.dataset.theme = snapshot.resolved;
  document.documentElement.style.colorScheme = snapshot.resolved;
}
function update(change: Partial<Snapshot>) {
  snapshot = { ...snapshot, ...change };
  apply();
  subscribers.forEach((listener) => listener());
}
async function nativeAppearance(preference?: AppearancePreference): Promise<Appearance> {
  const invoke = desktop.__TAURI_INTERNALS__?.invoke;
  if (!invoke) throw new Error("Desktop appearance is unavailable");
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    const result = await Promise.race([
      invoke("appearance", preference ? { preference } : {}),
      new Promise<never>((_, reject) => { timer = setTimeout(() => reject(new Error("Appearance response timed out")), 5000); }),
    ]);
    if (!validAppearance(result)) throw new Error("Invalid appearance response");
    return { preference: result.preference, resolved: result.resolved };
  } finally { if (timer) clearTimeout(timer); }
}

// Initialize before React renders. Native preferences are independent of the
// controller's loopback port; localStorage is only the standalone/test fallback.
export function initializeAppearance() {
  if (initialized) return;
  initialized = true;
  apply();
  const changedSystem = () => {
    if (!desktop.__TAURI_INTERNALS__ && snapshot.preference === "system") update({ resolved: systemAppearance() });
  };
  media.addEventListener("change", changedSystem);
  window.addEventListener("relay-appearance", (event) => {
    if (!desktop.__TAURI_INTERNALS__ || !validAppearance((event as CustomEvent).detail)) return;
    const next = (event as CustomEvent<Appearance>).detail;
    nativeEventVersion++;
    update({ preference: next.preference, resolved: next.resolved });
  });
  window.addEventListener("storage", (event) => {
    if (desktop.__TAURI_INTERNALS__ || (event.key !== storageKey && event.key !== null)) return;
    try { if (event.storageArea !== localStorage) return; } catch { return; }
    if (event.newValue !== null && !isPreference(event.newValue)) return;
    const preference = event.newValue ?? "system";
    update({ preference, resolved: resolve(preference), error: null });
  });
  if (desktop.__TAURI_INTERNALS__) {
    const current = ++revision;
    const events = nativeEventVersion;
    update({ busy: true });
    void nativeAppearance().then((result) => {
      if (revision === current) update({ ...(events === nativeEventVersion ? result : {}), busy: false, error: null });
    }).catch(() => {
      if (revision === current) update({ busy: false, error: "Desktop appearance is unavailable. Reload or update Relay and try again." });
    });
  }
}

function setPreference(preference: AppearancePreference) {
  if (!isPreference(preference) || snapshot.busy) return;
  if (desktop.__TAURI_INTERNALS__) {
    const current = ++revision;
    const events = nativeEventVersion;
    update({ busy: true, error: null });
    void nativeAppearance(preference).then((result) => {
      if (revision === current) update({ ...(events === nativeEventVersion ? result : {}), busy: false, error: null });
    }).catch(() => {
      if (revision === current) update({ busy: false, error: "Could not confirm the appearance change. Reload Relay or choose the setting again." });
    });
    return;
  }
  let error: string | null = null;
  try { localStorage.setItem(storageKey, preference); } catch { error = "Appearance changed for this window, but could not be saved on this device."; }
  update({ preference, resolved: resolve(preference), error });
}

export function dismissAppearanceError() { update({ error: null }); }
const subscribe = (listener: () => void) => { subscribers.add(listener); return () => { subscribers.delete(listener); }; };
const getSnapshot = () => snapshot;
export function useAppearance() {
  return { ...useSyncExternalStore(subscribe, getSnapshot, getSnapshot), setPreference };
}
