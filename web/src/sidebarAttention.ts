import { useEffect, useRef, useState } from "react";

type Preference = { collapsed: boolean };
type DesktopWindow = Window & {
  __RELAY_SIDEBAR_ATTENTION__?: unknown;
  __TAURI_INTERNALS__?: { invoke: (command: string, args?: Record<string, unknown>) => Promise<unknown> };
};
const storageKey = "relay.attention-collapsed.v1";
const desktop = window as DesktopWindow;
function valid(value: unknown): value is Preference {
  return !!value && typeof value === "object" && typeof (value as Preference).collapsed === "boolean";
}
function initialCollapsed() {
  if (valid(desktop.__RELAY_SIDEBAR_ATTENTION__)) return desktop.__RELAY_SIDEBAR_ATTENTION__.collapsed;
  try { return localStorage.getItem(storageKey) === "true"; } catch { return false; }
}

export function useSidebarAttention() {
  const [collapsed, setCollapsed] = useState(initialCollapsed);
  const [error, setError] = useState<string | null>(null);
  const current = useRef(collapsed);
  const revision = useRef(0);
  const saves = useRef(Promise.resolve());
  useEffect(() => {
    const invoke = desktop.__TAURI_INTERNALS__?.invoke;
    if (!invoke) return;
    let active = true;
    const started = revision.current;
    void invoke("sidebar_attention").then((result) => {
      if (!valid(result)) throw new Error("Invalid sidebar preference");
      if (active && started === revision.current) {
        current.current = result.collapsed;
        setCollapsed(result.collapsed);
      }
    }).catch(() => {
      if (active && started === revision.current) setError("Could not load your sidebar preference. Toggle to save it again.");
    });
    return () => { active = false; };
  }, []);

  const toggle = () => {
    const next = !current.current;
    current.current = next;
    const change = ++revision.current;
    setCollapsed(next);
    setError(null);
    const invoke = desktop.__TAURI_INTERNALS__?.invoke;
    if (!invoke) {
      try { localStorage.setItem(storageKey, String(next)); }
      catch { setError("Changed for this window. Could not save your sidebar preference."); }
      return;
    }
    // Native storage survives controller port changes. Serialize quick toggles
    // so a slow earlier write cannot replace the user's latest choice.
    const timer = setTimeout(() => {
      if (change === revision.current) setError("Changed for this window. Saving your sidebar preference is taking longer than expected.");
    }, 5000);
    saves.current = saves.current.then(async () => {
      try {
        const result = await invoke("sidebar_attention", { collapsed: next });
        if (!valid(result) || result.collapsed !== next) throw new Error("Sidebar preference was not confirmed");
        if (change === revision.current) setError(null);
      } catch {
        if (change === revision.current) setError("Changed for this window. Could not save your sidebar preference.");
      } finally { clearTimeout(timer); }
    });
  };
  return { collapsed, toggle, error };
}
