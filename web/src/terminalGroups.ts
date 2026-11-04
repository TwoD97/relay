import { useEffect, useRef, useState } from "react";
import type { FleetState, TerminalPaneRef } from "./types";

export type TerminalGroup = {
  id: string; title: string; hostName: string; workspace: string;
  panes: (TerminalPaneRef & { createdAt: string })[]; collapsed: boolean;
};
type Snapshot = { revision: number; groups: TerminalGroup[] };
type Change = { group: TerminalGroup } | { remove: string };
const storageKey = "relay.terminal-groups.v1";
const native = () => (window as Window & { __TAURI_INTERNALS__?: { invoke: (name: string, args?: Record<string, unknown>) => Promise<unknown> } }).__TAURI_INTERNALS__?.invoke;
const field = (value: unknown, max = 256): value is string => typeof value === "string" && value.length > 0 && value.length <= max && !/\p{Cc}/u.test(value);
export function validGroup(value: unknown): value is TerminalGroup {
  if (!value || typeof value !== "object") return false;
  const g = value as TerminalGroup;
  return typeof g.id === "string" && /^[a-f0-9-]{36}$/.test(g.id) && field(g.title) && field(g.hostName) && field(g.workspace) && typeof g.collapsed === "boolean"
    && Array.isArray(g.panes) && g.panes.length > 0 && g.panes.length <= 4
    && g.panes.every((p) => p && field(p.host) && field(p.session) && field(p.createdAt))
    && new Set(g.panes.map((p) => JSON.stringify([p.host, p.session]))).size === g.panes.length;
}
function valid(value: unknown): value is Snapshot {
  if (!value || typeof value !== "object") return false;
  const s = value as Snapshot;
  return Number.isSafeInteger(s.revision) && s.revision >= 0 && Array.isArray(s.groups) && s.groups.length <= 32 && s.groups.every(validGroup) && new Set(s.groups.map((g) => g.id)).size === s.groups.length;
}
function changed(groups: TerminalGroup[], change: Change) {
  return "remove" in change ? groups.filter((g) => g.id !== change.remove) : groups.some((g) => g.id === change.group.id) ? groups.map((g) => g.id === change.group.id ? change.group : g) : [...groups, change.group];
}
function browserRead(): Snapshot {
  const raw = localStorage.getItem(storageKey);
  const value: unknown = raw ? JSON.parse(raw) : { revision: 0, groups: [] };
  if (!valid(value)) throw Error("Saved terminal groups are invalid.");
  return value;
}
async function read(): Promise<Snapshot> {
  const invoke = native();
  const value = invoke ? await invoke("terminal_groups") : browserRead();
  if (!valid(value)) throw Error("Saved terminal groups could not be verified.");
  return value;
}
async function save(revision: number, change: Change): Promise<Snapshot> {
  const invoke = native();
  let value: unknown;
  if (invoke) value = await invoke("terminal_groups", { change: { revision, ...change } });
  else {
    const current = browserRead();
    if (current.revision !== revision) throw Error("Terminal groups changed in another window. Reload saved groups before editing.");
    value = { revision: revision + 1, groups: changed(current.groups, change) };
    if (!valid(value)) throw Error("You can save up to 32 terminal groups.");
    localStorage.setItem(storageKey, JSON.stringify(value));
  }
  if (!valid(value) || value.revision !== revision + 1) throw Error("Saving terminal groups was not confirmed.");
  return value;
}
export const matchingGroupSession = (state: FleetState, pane: TerminalPaneRef) => state.runtimes[pane.host]?.sessions.find((s) => s.id === pane.session && s.createdAt === pane.createdAt);
export function groupTitle(group: TerminalGroup, state: FleetState) { return matchingGroupSession(state, group.panes[0])?.title ?? group.title; }

export function useTerminalGroups() {
  const [groups, setGroups] = useState<TerminalGroup[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(0);
  const current = useRef<Snapshot>({ revision: 0, groups: [] });
  const visible = useRef<TerminalGroup[]>([]);
  const blocked = useRef(true), loading = useRef(false), queued = useRef(0);
  const chain = useRef(Promise.resolve());
  const mounted = useRef(true);
  const reload = async () => {
    if (queued.current || loading.current) return;
    loading.current = true;
    try {
      const value = await read();
      if (!mounted.current) return;
      current.current = value; visible.current = value.groups; blocked.current = false;
      setGroups(value.groups); setError(null);
    } catch (e) {
      if (mounted.current) { blocked.current = true; setError(`Could not load saved terminal groups. ${String(e instanceof Error ? e.message : e)}`); }
    } finally { loading.current = false; }
  };
  useEffect(() => {
    mounted.current = true; void reload();
    const refresh = () => { if (!blocked.current) void reload(); };
    window.addEventListener("focus", refresh); window.addEventListener("storage", refresh);
    return () => { mounted.current = false; window.removeEventListener("focus", refresh); window.removeEventListener("storage", refresh); };
  }, []);
  const mutate = (change: Change) => {
    const next = changed(visible.current, change);
    if (!valid({ revision: current.current.revision, groups: next })) { setError("You can save up to 32 terminal groups with four panes each."); return; }
    visible.current = next; setGroups(next);
    if (blocked.current || loading.current) { setError("This layout is open but not saved. Reload saved groups before making another change."); return; }
    queued.current++; setPending(queued.current);
    const timer = setTimeout(() => { if (mounted.current) setError("This layout is open. Saving terminal groups is taking longer than expected; no write will be retried automatically."); }, 5000);
    chain.current = chain.current.then(async () => {
      if (blocked.current) return;
      try {
        const result = await save(current.current.revision, change);
        current.current = result;
        if (mounted.current) setError(null);
      } catch (e) {
        blocked.current = true;
        if (mounted.current) setError(`This layout is open but saving was not confirmed. No write was retried. ${String(e instanceof Error ? e.message : e)}`);
      }
    }).finally(() => { clearTimeout(timer); queued.current--; if (mounted.current) setPending(queued.current); });
  };
  return { groups, error, pending, ready: !blocked.current && !loading.current, revision: current.current.revision, reload, upsert: (group: TerminalGroup) => mutate({ group }), remove: (id: string) => mutate({ remove: id }) };
}
